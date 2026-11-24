package operations

import (
	"context"
	"fmt"
	"math"
	"strings"

	"issue-desk-intake/server/internal/fleet"
)

// SlowIncidentDecision is the result of examining a slow incident. Cause identifies
// the confirmed defect, rather than an instruction to perform an operation.
type SlowIncidentDecision struct {
	Problem  bool
	Cause    string
	DirectPR bool
}

// SlowIncidentFiling is the handoff to the future filing implementation. It is
// not authorization to send: ValidateSlowIncidentFiling must be called again.
type SlowIncidentFiling struct {
	IncidentID, IncidentKey, Cause, Project, WorkKey string
	Number                                           int64
	ElapsedSeconds                                   float64
	DirectPR                                         bool
}

// JudgeSlowIncident is deliberately separate from the generic incident actions;
// startup integration and filing are owned by subsequent changes.
func (a *IncidentActions) JudgeSlowIncident(ctx context.Context, id string, judge func(context.Context, map[string]any) (SlowIncidentDecision, error)) (*SlowIncidentFiling, error) {
	if judge == nil {
		return nil, fmt.Errorf("遅延有事の判定先がありません")
	}
	row, detail, err := a.slowIncidentSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if row["slowJudgement"] != nil {
		return nil, fmt.Errorf("判定済みの有事です")
	}
	decision, err := judge(ctx, map[string]any{"incident": redactIncidentData(row), "detail": redactIncidentData(detail)})
	if err != nil {
		return nil, err
	}
	if !decision.Problem {
		// Recheck the source before recording even a no-problem judgement.
		if _, _, err = a.slowIncidentSnapshot(ctx, id); err != nil {
			return nil, err
		}
		return nil, a.Service.ResolveSlowWorkNoProblem(ctx, id, row)
	}
	cause := strings.TrimSpace(decision.Cause)
	if !incidentText(cause, 4000) {
		return nil, fmt.Errorf("原因を確認できません")
	}
	target := fleet.Object(row["target"])
	filing := &SlowIncidentFiling{IncidentID: id, IncidentKey: fleet.Text(row["key"]), Cause: cause, Project: fleet.Text(target["project"]), Number: int64(fleet.Number(target["number"])), WorkKey: fleet.Text(target["workKey"]), ElapsedSeconds: fleet.Number(target["elapsedSeconds"]), DirectPR: decision.DirectPR}
	if err = a.validateSlowFiling(ctx, filing, false); err != nil {
		return nil, err
	}
	err = a.Service.changeIncident(ctx, id, func(r map[string]any) error {
		if !slowRowMatches(r, filing) || r["endedAt"] != nil || r["slowJudgement"] != nil {
			return fmt.Errorf("有事が判定中に変更されました")
		}
		r["slowJudgement"] = "problem"
		r["slowCause"] = filing.Cause
		r["slowDirectPR"] = filing.DirectPR
		a.Service.incidentNote(r, "不具合", "遅延作業の不具合を判定しました。起票は後続処理で行います。")
		return nil
	})
	if err != nil {
		return nil, err
	}
	return filing, nil
}

func slowRowMatches(row map[string]any, f *SlowIncidentFiling) bool {
	if row == nil || f == nil {
		return false
	}
	t := fleet.Object(row["target"])
	return row["kind"] == "slow" && fleet.Text(row["id"]) == f.IncidentID && fleet.Text(row["key"]) == f.IncidentKey && f.IncidentKey == "slow-"+f.WorkKey && f.WorkKey != "" && fleet.Text(t["workKey"]) == f.WorkKey && fleet.Text(t["project"]) == f.Project && f.Project != "" && fleet.Number(t["number"]) == float64(f.Number) && f.Number > 0 && fleet.Number(t["elapsedSeconds"]) == f.ElapsedSeconds && f.ElapsedSeconds > 0 && !math.IsInf(f.ElapsedSeconds, 0) && !math.IsNaN(f.ElapsedSeconds)
}

func (a *IncidentActions) slowIncidentSnapshot(ctx context.Context, id string) (map[string]any, map[string]any, error) {
	value, err := a.Service.OperationalSetting(ctx, "incident-deadline")
	if err != nil {
		return nil, nil, err
	}
	row := incidentRow(fleet.List(value), id)
	if row == nil || row["kind"] != "slow" || row["endedAt"] != nil || row["state"] != "expired" {
		return nil, nil, fmt.Errorf("判定可能な遅延有事ではありません")
	}
	target := fleet.Object(row["target"])
	detail, err := a.read(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	if (detail == nil && fleet.Number(target["number"]) > 0) || detail["stopped"] == true {
		return nil, nil, fmt.Errorf("対象または停止状態を確認できません")
	}
	return row, detail, nil
}

func (a *IncidentActions) validateSlowFiling(ctx context.Context, f *SlowIncidentFiling, saved bool) error {
	if f == nil || !incidentText(f.Cause, 4000) {
		return fmt.Errorf("起票の原因を確認できません")
	}
	row, _, err := a.slowIncidentSnapshot(ctx, f.IncidentID)
	if err != nil {
		return err
	}
	if !slowRowMatches(row, f) || (saved && (row["slowJudgement"] != "problem" || row["slowCause"] != f.Cause || row["slowDirectPR"] != f.DirectPR)) {
		return fmt.Errorf("起票対象を照合できません")
	}
	if a.ValidateSlowSource != nil {
		return a.ValidateSlowSource(ctx, f)
	}
	return nil
}

// ValidateSlowIncidentFiling re-reads the incident and target immediately before
// a future sender reserves and sends; it never sends or clears a stop itself.
func (a *IncidentActions) ValidateSlowIncidentFiling(ctx context.Context, filing *SlowIncidentFiling) error {
	return a.validateSlowFiling(ctx, filing, true)
}
