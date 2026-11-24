package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/usage"
)

// SlowIncidentSender supplies the dedicated filing entry and a lookup-only
// recovery entry. Recovery must never create an issue when no intent exists.
type SlowIncidentSender interface {
	FileSlowIncident(context.Context, *SlowIncidentFiling, func(context.Context, *SlowIncidentFiling) error) (map[string]any, error)
	RecoverSlowIncident(context.Context, *SlowIncidentFiling) (map[string]any, error)
}

// SlowIncidentSubmissionID identifies both the reservation and filing marker.
func SlowIncidentSubmissionID(incident, cause string) string {
	b, _ := json.Marshal([]string{incident, cause})
	sum := sha256.Sum256(b)
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// FileJudgedSlowIncident is the handoff for the parent automation. The caller
// supplies the result of JudgeSlowIncident and the dedicated filing service.
// A persisted reservation is never treated as permission to post again.
func (a *IncidentActions) FileJudgedSlowIncident(ctx context.Context, f *SlowIncidentFiling, sender SlowIncidentSender) (map[string]any, error) {
	if f == nil || sender == nil {
		return nil, errors.New("遅延有事の起票先がありません")
	}
	snapshot := *f
	if err := a.ValidateSlowIncidentFiling(ctx, &snapshot); err != nil {
		return nil, err
	}
	id := SlowIncidentSubmissionID(snapshot.IncidentID, snapshot.Cause)
	reserved := false
	err := a.Service.changeIncident(ctx, snapshot.IncidentID, func(r map[string]any) error {
		if !slowRowMatches(r, &snapshot) || r["state"] != "expired" || r["endedAt"] != nil || r["slowJudgement"] != "problem" || r["slowCause"] != snapshot.Cause || r["slowDirectPR"] != snapshot.DirectPR {
			return errors.New("起票対象が変更されました")
		}
		if existing := fleet.Object(r["slowFiling"]); existing != nil {
			if existing["id"] != id || existing["cause"] != snapshot.Cause {
				return errors.New("起票予約が一致しません")
			}
			return nil
		}
		r["slowFiling"] = map[string]any{"id": id, "cause": snapshot.Cause, "result": "unknown", "directPR": snapshot.DirectPR}
		r["acknowledgedAt"] = nil
		note := "送信ID " + id + "。結果を確認できない場合は再送しません。"
		if snapshot.DirectPR {
			note += " 直接PRが必要です。"
		}
		a.Service.incidentNote(r, "起票予約", note)
		reserved = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	var issue map[string]any
	var sendErr error
	validationFailed := false
	if reserved {
		issue, sendErr = sender.FileSlowIncident(ctx, &snapshot, func(ctx context.Context, current *SlowIncidentFiling) error {
			e := a.ValidateSlowIncidentFiling(ctx, current)
			if e != nil {
				validationFailed = true
			}
			return e
		})
	} else {
		issue, sendErr = sender.RecoverSlowIncident(ctx, &snapshot)
	}
	return a.saveSlowFilingResult(ctx, &snapshot, issue, sendErr, reserved, validationFailed)
}

// RecoverSlowIncidentFiling can be called after a restart or after the source
// work has ended. It only reconciles the saved reservation; it cannot submit.
func (a *IncidentActions) RecoverSlowIncidentFiling(ctx context.Context, incidentID string, sender SlowIncidentSender) (map[string]any, error) {
	if sender == nil {
		return nil, errors.New("遅延有事の照合先がありません")
	}
	value, err := a.Service.OperationalSetting(ctx, "incident-deadline")
	if err != nil {
		return nil, err
	}
	r := incidentRow(fleet.List(value), incidentID)
	if r == nil || r["slowJudgement"] != "problem" {
		return nil, errors.New("保存済み判定がありません")
	}
	target := fleet.Object(r["target"])
	f := &SlowIncidentFiling{IncidentID: incidentID, IncidentKey: fleet.Text(r["key"]), Cause: fleet.Text(r["slowCause"]), Project: fleet.Text(target["project"]), Number: int64(fleet.Number(target["number"])), WorkKey: fleet.Text(target["workKey"]), ElapsedSeconds: fleet.Number(target["elapsedSeconds"]), DirectPR: r["slowDirectPR"] == true}
	record := fleet.Object(r["slowFiling"])
	if !slowRowMatches(r, f) || !incidentText(f.Cause, 4000) || record == nil || record["id"] != SlowIncidentSubmissionID(incidentID, f.Cause) || record["cause"] != f.Cause || record["directPR"] != f.DirectPR {
		return nil, errors.New("起票予約を照合できません")
	}
	issue, err := sender.RecoverSlowIncident(ctx, f)
	return a.saveSlowFilingResult(ctx, f, issue, err, false, false)
}

func (a *IncidentActions) saveSlowFilingResult(ctx context.Context, snapshot *SlowIncidentFiling, issue map[string]any, sendErr error, reserved, validationFailed bool) (map[string]any, error) {
	id := SlowIncidentSubmissionID(snapshot.IncidentID, snapshot.Cause)
	if sendErr == nil && (issue == nil || fleet.Number(issue["number"]) < 1) {
		sendErr = errors.New("起票結果を確認できません")
	}
	// A lookup cannot tell whether the reservation owner is still sending.
	// Leave the reservation untouched until the owner reports or a lookup finds it.
	if !reserved && sendErr != nil {
		return issue, sendErr
	}
	result, message := "success", "起票しました。"
	if sendErr != nil {
		result, message = "unknown", "起票送信の成否を確認できません。再送しません。"
		var ge *gitea.Error
		if reserved && (validationFailed || errors.As(sendErr, &ge) && !ge.UnknownOutcome) {
			result, message = "failed", "起票送信に失敗しました。再送しません。"
		}
		message += " " + usage.Redact(sendErr.Error())
	}
	if snapshot.DirectPR {
		message += " 直接PRが必要です。"
	}
	// Preserve the reservation on cancellation or a failed result write.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saveErr := a.Service.changeIncident(saveCtx, snapshot.IncidentID, func(r map[string]any) error {
		record := fleet.Object(r["slowFiling"])
		if record == nil || record["id"] != id {
			return errors.New("起票予約を確認できません")
		}
		// Never downgrade a confirmed issue or duplicate a history entry.
		if record["result"] == "success" || record["result"] == "failed" || record["reported"] == true && result == "unknown" {
			return nil
		}
		record["result"], record["reported"], record["message"] = result, true, message
		if issue != nil {
			record["issueNumber"] = issue["number"]
		}
		a.Service.incidentNote(r, "起票結果", fmt.Sprintf("送信ID %s: %s", id, message))
		return nil
	})
	if saveErr != nil {
		return issue, saveErr
	}
	return issue, sendErr
}

func slowFilingNeedsAttention(r map[string]any) bool {
	f := fleet.Object(r["slowFiling"])
	return r["kind"] == "slow" && f != nil && (f["result"] != "success" || f["directPR"] == true)
}
