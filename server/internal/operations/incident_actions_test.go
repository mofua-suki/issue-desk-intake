package operations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

func TestSlowIncidentJudgementAndRecovery(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if err := s.mutateIncidents(ctx, func(rows *[]any) error {
		*rows = []any{map[string]any{"id": id, "key": "slow-job-1", "kind": "slow", "state": "expired", "target": map[string]any{"project": "owner/repo", "number": 1, "workKey": "job-1", "elapsedSeconds": 120}, "history": []any{}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sends := 0
	a := IncidentActions{Service: s, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1}, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	f, err := a.JudgeSlowIncident(ctx, id, func(context.Context, map[string]any) (SlowIncidentDecision, error) {
		return SlowIncidentDecision{}, nil
	})
	if err != nil || f != nil {
		t.Fatalf("%v %v", f, err)
	}
	value, err := s.OperationalSetting(ctx, "incident-deadline")
	if err != nil {
		t.Fatal(err)
	}
	row := incidentRow(fleet.List(value), id)
	count := 0
	for _, v := range fleet.List(row["history"]) {
		if fleet.Object(v)["event"] == "不具合なし" {
			count++
		}
	}
	if row["slowJudgement"] != "no_problem" || row["endedAt"] == nil || count != 1 || sends != 0 || len(fleet.List(row["actions"])) != 0 {
		t.Fatalf("row=%v sends=%d", row, sends)
	}
}

func TestSlowIncidentFilingBoundary(t *testing.T) {
	t.Run("担当引き継ぎは送信しない", func(t *testing.T) {
		s := fixture(t)
		s.PostgreSQL = postgres(t)
		ctx := context.Background()
		id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		if err := s.mutateIncidents(ctx, func(rows *[]any) error {
			*rows = []any{map[string]any{"id": id, "key": "slow-job-1", "kind": "slow", "state": "expired", "target": map[string]any{"project": "owner/repo", "number": 1, "workKey": "job-1", "elapsedSeconds": 120}, "history": []any{}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sends := 0
		a := IncidentActions{Service: s, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
			return map[string]any{"project": "owner/repo", "number": 1}, nil
		}, Judge: func(context.Context, []any) (map[string]any, error) {
			return map[string]any{"actions": []any{map[string]any{"action": "handover", "item": "slow-job-1", "reason": "遅延", "text": "引き継ぎ", "evidence": "確認", "resolved": false, "requiresUser": false}}}, nil
		}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
		if err := a.Execute(ctx, id); err != nil || sends != 0 {
			t.Fatalf("err=%v sends=%d", err, sends)
		}
	})
	for _, mode := range []string{"valid", "monitor_stopped", "full_test", "main_broken", "stage_watch", "stall", "foreign", "stopped", "stop_after_judge", "changed_work"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			s.PostgreSQL = postgres(t)
			ctx := context.Background()
			id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			kind := "slow"
			switch mode {
			case "monitor_stopped", "full_test", "main_broken", "stage_watch", "stall":
				kind = mode
			}
			if err := s.mutateIncidents(ctx, func(rows *[]any) error {
				*rows = []any{map[string]any{"id": id, "key": "slow-job-1", "kind": kind, "state": "expired", "target": map[string]any{"project": "owner/repo", "number": 1, "workKey": "job-1", "elapsedSeconds": 120}, "history": []any{}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stopped := mode == "stopped"
			sends := 0
			a := IncidentActions{Service: s, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
				project := "owner/repo"
				if mode == "foreign" {
					project = "owner/other"
				}
				return map[string]any{"project": project, "number": 1, "stopped": stopped}, nil
			}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
			f, err := a.JudgeSlowIncident(ctx, id, func(context.Context, map[string]any) (SlowIncidentDecision, error) {
				return SlowIncidentDecision{Problem: true, Cause: "defect", DirectPR: true}, nil
			})
			valid := mode == "valid" || mode == "stop_after_judge" || mode == "changed_work"
			if (err == nil) != valid {
				t.Fatalf("%v %v", f, err)
			}
			if valid {
				if err := s.ResolveSlowWorkNoProblem(ctx, id); err == nil {
					t.Fatal("判定済みの不具合を上書きしました")
				}
				if f.WorkKey != "job-1" || f.ElapsedSeconds != 120 || !f.DirectPR || f.Cause != "defect" {
					t.Fatal(f)
				}
				if mode == "stop_after_judge" {
					stopped = true
				}
				if mode == "changed_work" {
					f.WorkKey = "job-2"
				}
				err = a.ValidateSlowIncidentFiling(ctx, f)
				if (err == nil) != (mode == "valid") {
					t.Fatalf("validation=%v", err)
				}
			}
			if sends != 0 {
				t.Fatal(sends)
			}
		})
	}
}
