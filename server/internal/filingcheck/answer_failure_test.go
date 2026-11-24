package filingcheck

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/pgtest"
	"path/filepath"
	"strings"
	"testing"
)

type failingAnswerRunner struct {
	calls      int
	diagnostic bool
}

func (r *failingAnswerRunner) Call(_ context.Context, req ai.Request) (ai.Answer, error) {
	r.calls++
	if err := req.BeforeCall(); err != nil {
		return ai.Answer{}, err
	}
	d := ai.Diagnostic{}
	if r.diagnostic {
		code := 7
		first := int64(12)
		d = ai.Diagnostic{ID: "1234567890abcdef", Kind: "exit_nonzero", Stage: "exit", ElapsedMs: 42, FirstOutputMs: &first, StdoutBytes: 30, StderrBytes: 40, ExitCode: &code, FinalEvent: "turn.failed"}
	}
	return ai.Answer{Diagnostic: d}, &ai.Failure{Kind: "execution", Message: "secret https://example.invalid 短い会話"}
}
func TestAnswerFailureRecordsRoundTrip(t *testing.T) {
	for _, diagnostic := range []bool{true, false} {
		t.Run(map[bool]string{true: "diagnostic", false: "no_diagnostic"}[diagnostic], func(t *testing.T) {
			runner := &failingAnswerRunner{diagnostic: diagnostic}
			s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { return nil, nil })
			modelDB := pgtest.New(t)
			modelDB.SetMaxOpenConns(1)
			if _, err := modelDB.Exec(`CREATE TEMP TABLE settings (key text, value jsonb, expires_at timestamptz)`); err != nil {
				t.Fatal(err)
			}
			schema, _ := filepath.Abs(filepath.Join("..", "..", "schemas"))
			aiService, err := consultationai.New(consultationai.Options{Config: func(context.Context) (map[string]any, error) { return map[string]any{"model": "gpt-6.1-terra"}, nil }, Models: &models.Service{DB: modelDB}, Runner: runner, Gitea: &gitea.Client{}, Records: s.options.Records, RuntimeDir: t.TempDir(), SchemaDir: schema})
			if err != nil {
				t.Fatal(err)
			}
			s.options.Answer = aiService.Answer
			err = s.Check(t.Context(), d, "explicit_resend")
			if err == nil {
				t.Fatal("expected failure")
			}
			var typed *consultationai.AnswerFailure
			var af *ai.Failure
			if !errors.As(err, &typed) || !errors.As(err, &af) || af.Kind != "usage_unmeasured" || typed.Error() != errors.Unwrap(typed).Error() {
				t.Fatalf("error compatibility: %v", err)
			}
			if runner.calls != 1 {
				t.Fatalf("calls=%d", runner.calls)
			}
			saved, ok, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
			if err != nil || !ok {
				t.Fatal(err)
			}
			checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
			check := obj(checks[0])
			if check["result"] != "ai_failed" || fleet.WholeNumber(check["revision"]) != 1 || fleet.WholeNumber(check["phase"]) != 0 {
				t.Fatalf("check=%v", check)
			}
			if diagnostic {
				rows := fleet.Elements(obj(check["answerFailure"])["diagnostics"])
				if len(rows) != 1 || obj(rows[0])["kind"] != "exit_nonzero" || fleet.WholeNumber(obj(rows[0])["exitCode"]) != 7 {
					t.Fatalf("diagnostics=%v", rows)
				}
			}
			text := string(raw(check))
			for _, secret := range []string{"secret", "https://example.invalid", "短い会話"} {
				if strings.Contains(text, secret) {
					t.Fatalf("leaked %q", secret)
				}
			}
			saved["revision"] = 2
			_, err = s.options.Records.Change(t.Context(), "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return saved, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Check(t.Context(), saved, "explicit_resend"); err == nil {
				t.Fatal("expected second failure")
			}
			again, _, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			calls := obj(again["filingCheckCalls"])
			checks = fleet.Elements(calls["checks"])
			if len(checks) != 2 || fleet.WholeNumber(calls["count"]) != 1 || fleet.WholeNumber(obj(checks[1])["revision"]) != 2 || runner.calls != 2 {
				t.Fatalf("history=%v", calls)
			}
		})
	}
}
func TestAnswerFailureAllowlist(t *testing.T) {
	failure := &consultationai.AnswerFailure{Err: context.Canceled, Diagnostics: []any{map[string]any{"kind": "exit_nonzero", "id": "secret", "model": "https://example.invalid", "finalEvent": "短い会話", "stdoutBytes": 5}}, MeasurementHold: map[string]any{"reason": "missing", "originalCause": "secret"}}
	if !errors.Is(failure, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	data := string(raw(answerFailure(Record{}, failure)))
	for _, s := range []string{"secret", "https://example.invalid", "短い会話"} {
		if strings.Contains(data, s) {
			t.Fatal(data)
		}
	}
}

func TestAnswerFailurePhaseAndSaveFence(t *testing.T) {
	t.Run("phase", func(t *testing.T) {
		calls := 0
		s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
			calls++
			if calls == 1 {
				out := allowed()
				out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
				return out, nil
			}
			return nil, &consultationai.AnswerFailure{Err: errors.New("failure"), Diagnostics: []any{map[string]any{"id": "1234567890abcdef", "kind": "exit_nonzero"}}}
		})
		if err := s.Check(t.Context(), d); err == nil {
			t.Fatal("expected failure")
		}
		saved, _, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
		if err != nil {
			t.Fatal(err)
		}
		checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
		if len(checks) != 2 || fleet.WholeNumber(obj(checks[1])["phase"]) != 1 || obj(checks[1])["result"] != "ai_failed" || obj(checks[1])["answerFailure"] == nil {
			t.Fatalf("checks=%v", checks)
		}
	})
	t.Run("fence", func(t *testing.T) {
		var s *Service
		var d Record
		s, d, _ = fixture(t, func(ctx context.Context, _ map[string]any) (map[string]any, error) {
			_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) { current["revision"] = 2; return current, nil })
			if err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("original failure")
		})
		err := s.Check(t.Context(), d)
		if err == nil || strings.Contains(err.Error(), "original failure") {
			t.Fatalf("save failure not returned: %v", err)
		}
	})
}

func TestCheckHistoryBoundAndDiagnosticModel(t *testing.T) {
	model := "new-model.2026"
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		return nil, &consultationai.AnswerFailure{Err: errors.New("failure"), Diagnostics: []any{map[string]any{"kind": "exit_nonzero", "model": model}}}
	})
	for revision := 1; revision <= checkHistoryLimit+6; revision++ {
		if revision > 1 {
			var err error
			d, _, err = s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			d["revision"] = revision
			if _, err = s.options.Records.Change(t.Context(), "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Check(t.Context(), d, "explicit_resend"); err == nil {
			t.Fatal("expected failure")
		}
		saved, _, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
		if err != nil {
			t.Fatal(err)
		}
		calls := obj(saved["filingCheckCalls"])
		checks := fleet.Elements(calls["checks"])
		if len(checks) > checkHistoryLimit || fleet.WholeNumber(calls["count"]) != 1 || fleet.WholeNumber(obj(checks[len(checks)-1])["revision"]) != int64(revision) {
			t.Fatalf("revision %d: %v", revision, calls)
		}
		if revision > checkHistoryLimit && fleet.WholeNumber(obj(checks[0])["revision"]) != int64(revision-checkHistoryLimit+1) {
			t.Fatalf("oldest: %v", checks[0])
		}
		row := obj(fleet.Elements(obj(obj(checks[len(checks)-1])["answerFailure"])["diagnostics"])[0])
		if row["model"] != model {
			t.Fatalf("model: %v", row)
		}
	}
	for _, invalid := range []string{"https://example.invalid", "short conversation", "会話断片", strings.Repeat("a", 65)} {
		model = invalid
		d, _, _ = s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
		d["revision"] = fleet.WholeNumber(d["revision"]) + 1
		if _, err := s.options.Records.Change(t.Context(), "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
		if err := s.Check(t.Context(), d, "explicit_resend"); err == nil {
			t.Fatal("expected failure")
		}
		saved, _, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
		if err != nil {
			t.Fatal(err)
		}
		checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
		row := obj(fleet.Elements(obj(obj(checks[len(checks)-1])["answerFailure"])["diagnostics"])[0])
		if _, exists := row["model"]; exists {
			t.Fatalf("unsafe model saved: %v", row)
		}
	}
}

func TestEmptyAnswerFailure(t *testing.T) {
	failure := &consultationai.AnswerFailure{}
	if failure.Error() == "" || errors.Unwrap(failure) != nil {
		t.Fatal("empty failure should be safe")
	}
}
