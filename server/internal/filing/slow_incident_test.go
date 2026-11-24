package filing

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/operations"
	"strings"
	"sync"
	"testing"
)

func TestSlowIncidentFilingRecovery(t *testing.T) {
	t.Run("incident recovery after work ends", slowIncidentFilingRecoveryIntegrated)
	ctx := context.Background()
	t.Run("dedicated entry recovery", func(t *testing.T) {
		s, api, f, _ := slowFilingFixture(t)
		validate := func(context.Context, *operations.SlowIncidentFiling) error { return nil }
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = s.FileSlowIncident(ctx, f, validate) }()
		}
		wg.Wait()
		if api.posts != 1 {
			t.Fatalf("posts=%d", api.posts)
		}
	})
	t.Run("dedicated entry uncertain", func(t *testing.T) {
		s, api, f, _ := slowFilingFixture(t)
		api.uncertain, api.lose = true, true
		validate := func(context.Context, *operations.SlowIncidentFiling) error { return nil }
		if _, err := s.FileSlowIncident(ctx, f, validate); err == nil {
			t.Fatal("expected unknown outcome")
		}
		if _, err := s.FileSlowIncident(ctx, f, validate); err == nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	valid := func(context.Context, slowIncidentIntent) error { return nil }
	intent := slowIncidentIntent{IncidentID: "incident", Cause: "defect", Project: "owner/app", Prepared: Prepared{Title: "delay", Body: "body"}}
	t.Run("duplicate and restart", func(t *testing.T) {
		s, api := fixture(t)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = s.fileSlowIncidentMode(ctx, intent, valid, false) }()
		}
		wg.Wait()
		if api.posts != 1 {
			t.Fatalf("posts = %d", api.posts)
		}
		restarted, err := New(ctx, s.options)
		if err != nil {
			t.Fatal(err)
		}
		issue, err := restarted.fileSlowIncidentMode(ctx, intent, valid, false)
		if err != nil || issue == nil || api.posts != 1 {
			t.Fatalf("issue=%v err=%v posts=%d", issue, err, api.posts)
		}
	})
	t.Run("lost response and missing issue", func(t *testing.T) {
		s, api := fixture(t)
		api.uncertain, api.lose = true, true
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil {
			t.Fatal("expected unknown outcome")
		}
		restarted, err := New(ctx, s.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.fileSlowIncidentMode(ctx, intent, valid, false); err == nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("lost response and found issue", func(t *testing.T) {
		s, api := fixture(t)
		api.uncertain = true
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil {
			t.Fatal("expected unknown outcome")
		}
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err != nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("foreign marker cannot reconcile", func(t *testing.T) {
		s, api := fixture(t)
		api.uncertain, api.lose = true, true
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil {
			t.Fatal("expected unknown outcome")
		}
		api.issues = []map[string]any{{"number": int64(99), "body": "<!-- issue-desk:" + slowIncidentID(intent.IncidentID, intent.Cause) + " -->", "user": map[string]any{"id": int64(11)}}}
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("duplicate markers cannot reconcile", func(t *testing.T) {
		s, api := fixture(t)
		api.uncertain, api.lose = true, true
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil {
			t.Fatal("expected unknown outcome")
		}
		body := "<!-- issue-desk:" + slowIncidentID(intent.IncidentID, intent.Cause) + " -->"
		api.issues = []map[string]any{{"number": int64(1), "body": body, "user": map[string]any{"id": int64(10)}}, {"number": int64(2), "body": body, "user": map[string]any{"id": int64(10)}}}
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("post result save failure", func(t *testing.T) {
		s, api := fixture(t)
		if _, err := s.options.Records.DB.ExecContext(ctx, `ALTER TABLE slow_incident_filings ADD CONSTRAINT reject_result CHECK (json NOT LIKE '%issueNumber%')`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
		if _, err := s.options.Records.DB.ExecContext(ctx, "ALTER TABLE slow_incident_filings DROP CONSTRAINT reject_result"); err != nil {
			t.Fatal(err)
		}
		restarted, err := New(ctx, s.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.fileSlowIncidentMode(ctx, intent, valid, false); err != nil || api.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("save failure", func(t *testing.T) {
		s, api := fixture(t)
		if _, err := s.options.Records.DB.ExecContext(ctx, "ALTER TABLE slow_incident_filings ADD CONSTRAINT reject_intent CHECK (false)"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.fileSlowIncidentMode(ctx, intent, valid, false); err == nil || api.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
}

func TestSlowIncidentFilingValidationRequired(t *testing.T) {
	ctx := context.Background()
	intent := slowIncidentIntent{IncidentID: "incident", Cause: "defect", Project: "owner/app", Prepared: Prepared{Title: "delay", Body: "body"}}
	t.Run("missing", func(t *testing.T) {
		s, api := fixture(t)
		if _, err := s.fileSlowIncidentMode(ctx, intent, nil, false); err == nil || api.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("before post", func(t *testing.T) {
		s, api := fixture(t)
		calls := 0
		validate := func(context.Context, slowIncidentIntent) error {
			calls++
			if calls == 2 {
				return errors.New("stopped")
			}
			return nil
		}
		if _, err := s.fileSlowIncidentMode(ctx, intent, validate, false); err == nil || calls != 2 || api.posts != 0 {
			t.Fatalf("err=%v calls=%d posts=%d", err, calls, api.posts)
		}
		restarted, err := New(ctx, s.options)
		if err != nil {
			t.Fatal(err)
		}
		issue, err := restarted.fileSlowIncidentMode(ctx, intent, validate, false)
		if err != nil || issue == nil || calls != 5 || api.posts != 1 {
			t.Fatalf("issue=%v err=%v calls=%d posts=%d", issue, err, calls, api.posts)
		}
	})
}

func slowFilingFixture(t *testing.T) (*Service, *memoryAPI, *operations.SlowIncidentFiling, map[string]any) {
	t.Helper()
	s, api := fixture(t)
	f := &operations.SlowIncidentFiling{IncidentID: "i1", IncidentKey: "slow-work-1", Cause: "defect", Project: "owner/app", Number: 42, WorkKey: "work-1", ElapsedSeconds: 120, DirectPR: true}
	saved := map[string]any{"id": f.IncidentID, "key": f.IncidentKey, "kind": "slow", "state": "expired", "slowJudgement": "problem", "slowCause": f.Cause, "slowDirectPR": true, "target": map[string]any{"project": f.Project, "number": float64(f.Number), "workKey": f.WorkKey, "elapsedSeconds": f.ElapsedSeconds}}
	s.options.IncidentLedger = func(context.Context) (any, error) { return []any{saved}, nil }
	return s, api, f, saved
}

func TestSlowIncidentFilingBoundary(t *testing.T) {
	t.Run("judged stopped target", slowIncidentFilingBoundaryIntegrated)
	for _, tc := range []struct {
		name    string
		change  func(map[string]any)
		stopped bool
	}{
		{"judgement", func(r map[string]any) { r["slowJudgement"] = "no_problem" }, false},
		{"cause", func(r map[string]any) { r["slowCause"] = "other" }, false},
		{"target", func(r map[string]any) { r["target"].(map[string]any)["number"] = float64(99) }, false},
		{"work", func(r map[string]any) { r["target"].(map[string]any)["workKey"] = "other" }, false},
		{"kind", func(r map[string]any) { r["kind"] = "full_test" }, false},
		{"stopped", func(map[string]any) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api, f, saved := slowFilingFixture(t)
			tc.change(saved)
			_, err := s.FileSlowIncident(context.Background(), f, func(context.Context, *operations.SlowIncidentFiling) error {
				if tc.stopped {
					return errors.New("stopped")
				}
				return nil
			})
			if err == nil || api.posts != 0 {
				t.Fatalf("err=%v posts=%d", err, api.posts)
			}
		})
	}
}

func TestSlowIncidentFilingStopBeforeSend(t *testing.T) {
	s, api, f, _ := slowFilingFixture(t)
	calls := 0
	_, err := s.FileSlowIncident(context.Background(), f, func(context.Context, *operations.SlowIncidentFiling) error {
		calls++
		if calls == 2 {
			return errors.New("stopped")
		}
		return nil
	})
	if err == nil || calls != 2 || api.posts != 0 {
		t.Fatalf("err=%v calls=%d posts=%d", err, calls, api.posts)
	}
	issue, err := s.FileSlowIncident(context.Background(), f, func(context.Context, *operations.SlowIncidentFiling) error { return nil })
	if err != nil || issue == nil || api.posts != 1 {
		t.Fatalf("issue=%v err=%v posts=%d", issue, err, api.posts)
	}
}

func TestSlowIncidentFilingBody(t *testing.T) {
	s, api, f, _ := slowFilingFixture(t)
	issue, err := s.FileSlowIncident(context.Background(), f, func(context.Context, *operations.SlowIncidentFiling) error { return nil })
	if err != nil || api.posts != 1 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
	body := fleet.Text(issue["body"])
	for _, want := range []string{`"priority":"high"`, "work-1", "120秒", "直接PRが要る"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}
