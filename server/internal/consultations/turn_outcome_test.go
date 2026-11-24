package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func outcomeSetup(t *testing.T, initial Record, turn func(context.Context, Record) (Record, error)) (*Service, *Engine) {
	t.Helper()
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	if initial != nil {
		_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
			for k, v := range initial {
				d[k] = v
			}
			return d, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: turn, Submit: func(context.Context, Record) (Record, error) {
		t.Error("unexpected automatic submission")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return s, e
}
func outcomeDraft(t *testing.T, s *Service) Record {
	t.Helper()
	d, err := s.Draft(context.Background(), todoID, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestTurnOutcomeStopsQuestionlessResult(t *testing.T) {
	for _, tc := range []struct {
		name      string
		questions any
		missing   bool
		children  any
	}{{"empty", []any{}, false, nil}, {"missing", nil, true, nil}, {"blank", []any{" ", "\t"}, false, nil}, {"emptySplitChildren", []any{}, false, []any{}}} {
		t.Run(tc.name, func(t *testing.T) {
			initial := Record{"consultationBudget": Record{"calls": 2, "weighted": 42}}
			if tc.children != nil {
				initial["splitChildren"] = tc.children
			}
			s, e := outcomeSetup(t, initial, func(context.Context, Record) (Record, error) {
				r := Record{"ready": false, "body": "保存本文", "reply": "保存応答"}
				if !tc.missing {
					r["questions"] = tc.questions
				}
				return r, nil
			})
			e.attempt(context.Background(), todoID)
			d := outcomeDraft(t, s)
			weighted, _ := integer(record(d["consultationBudget"])["weighted"])
			if d["status"] != "stalled" || d["ready"] != false || d["retryAt"] != nil || d["questions"] == nil || len(fleet.Items(d["questions"])) != 0 || text(d["error"]) == "" || d["body"] != "保存本文" || weighted != 42 || text(record(fleet.Items(d["messages"])[1])["text"]) != "保存応答" {
				t.Fatalf("unexpected result: %+v", d)
			}
		})
	}
}
func TestTurnOutcomePreservesActionableResult(t *testing.T) {
	s, e := outcomeSetup(t, nil, func(context.Context, Record) (Record, error) {
		return Record{"ready": false, "questions": []any{"確認？"}, "reply": "質問"}, nil
	})
	e.attempt(context.Background(), todoID)
	d := outcomeDraft(t, s)
	if d["status"] != "draft" || len(fleet.Items(d["questions"])) != 1 || d["error"] != nil {
		t.Fatalf("%+v", d)
	}
}
func TestTurnOutcomeExemptions(t *testing.T) {
	cases := []struct {
		name            string
		initial, result Record
		status          string
	}{
		{"hold", Record{"hold": true}, nil, "draft"}, {"parentSplit", Record{"parentSplit": "parent"}, nil, "draft"},
		{"budget", nil, Record{"consultationBudget": Record{"stopped": Record{"calls": 3}}}, "draft"},
		{"children", nil, Record{"consultationChildren": []any{Record{"id": "child"}}}, "draft"},
		{"savedSplitChildren", Record{"splitChildren": []any{"child"}}, nil, "draft"},
		{"newProject", nil, Record{"newProject": Record{"needed": true}}, "draft"},
		{"continue", nil, Record{"_continue": true, "context": Record{"marker": "kept"}}, "thinking"},
		{"handoff", nil, Record{"status": "project_handoff", "projectHandoff": Record{"project": "other"}}, "project_handoff"},
		{"parentConsultation", Record{"parentConsultation": "parent"}, nil, "stalled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e := outcomeSetup(t, tc.initial, func(context.Context, Record) (Record, error) {
				r := Record{"ready": false, "questions": []any{}, "reply": "応答"}
				for k, v := range tc.result {
					r[k] = v
				}
				return r, nil
			})
			e.attempt(context.Background(), todoID)
			d := outcomeDraft(t, s)
			if d["status"] != tc.status || text(record(fleet.Items(d["messages"])[1])["text"]) != "応答" {
				t.Fatalf("%+v", d)
			}
			switch tc.name {
			case "savedSplitChildren":
				if d["error"] != nil || d["ready"] != false || len(fleet.Items(d["splitChildren"])) != 1 || text(d["parentSplit"]) != "" {
					t.Fatal(d)
				}
			case "children":
				if len(fleet.Items(d["consultationChildren"])) != 1 {
					t.Fatal(d)
				}
			case "newProject":
				if record(d["newProject"])["needed"] != true {
					t.Fatal(d)
				}
			case "budget":
				if record(record(d["consultationBudget"])["stopped"]) == nil {
					t.Fatal(d)
				}
			case "continue":
				if record(d["carriedContext"])["marker"] != "kept" {
					t.Fatal(d)
				}
			case "handoff":
				if record(d["projectHandoff"])["project"] != "other" {
					t.Fatal(d)
				}
			}
		})
	}
}
func TestTurnOutcomeResumesAfterStop(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "reply"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			s, e := outcomeSetup(t, nil, func(context.Context, Record) (Record, error) {
				calls++
				if calls == 1 {
					return Record{"ready": false, "questions": []any{}}, nil
				}
				return Record{"ready": false, "questions": []any{"確認？"}, "reply": "再開"}, nil
			})
			ctx := context.Background()
			e.attempt(ctx, todoID)
			d := outcomeDraft(t, s)
			if d["status"] != "stalled" {
				t.Fatal(d)
			}
			input := Record{"text": "追加"}
			if retry {
				input = Record{"retry": true}
			}
			if _, err := s.Chat(ctx, todoID, input, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			e.attempt(ctx, todoID)
			d = outcomeDraft(t, s)
			if calls != 2 || d["status"] != "draft" || d["error"] != nil || len(fleet.Items(d["questions"])) != 1 {
				t.Fatalf("calls=%d draft=%+v", calls, d)
			}
		})
	}
}
