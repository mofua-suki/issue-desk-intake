package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

func TestFinalizeScopeRepair(t *testing.T) {
	for _, tc := range []struct {
		name             string
		count, corrected int
		limit            bool
		wantError        bool
		calls            int
	}{
		{"empty non filing", 0, -1, false, false, 1},
		{"filing zero repaired", 0, 1, false, false, 2},
		{"filing forty one repaired", 41, 40, false, false, 2},
		{"still invalid", 41, 41, false, true, 2},
		{"limit reached", 41, 1, true, true, 1},
		{"filing one", 1, 1, false, false, 1},
		{"filing forty", 40, 40, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
				calls++
				count := tc.count
				if calls > 1 {
					count = tc.corrected
				}
				paths := []any{}
				for i := 0; i < count; i++ {
					paths = append(paths, fmt.Sprintf("src/%d.go", i))
				}
				v := reply()
				v["changeScope"] = Record{"known": true, "paths": paths}
				if tc.corrected != -1 {
					v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "本文", []any{}
				}
				return measuredAnswer(v), nil
			})
			// Avoid the separate existing-request read so calls count scope repairs only.
			s.options.Reference = nil
			if tc.limit {
				d["formatRepairs"] = Record{"total": 8}
			}
			got, err := s.Turn(context.Background(), d)
			if (err != nil) != tc.wantError || calls != tc.calls {
				t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
			}
			if tc.wantError {
				var failure *ai.Failure
				if !errors.As(err, &failure) || failure.Kind != "reply_check" {
					t.Fatalf("unexpected failure: %v", err)
				}
			}
			if tc.corrected == -1 && (got["reply"] != "質問です" || len(fleet.Elements(got["questions"])) != 1 || fleet.Object(got["changeScope"])["known"] != false) {
				t.Fatalf("result=%v", got)
			}
		})
	}
}

func TestTransferUnavailableDocumentReadHolds(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		if calls == 1 {
			v["readFiles"] = []any{Record{"file": "issue-3.md", "mode": "full"}}
		} else {
			if !strings.Contains(r.Input, "missing") {
				t.Errorf("read refusal absent from input")
			}
			v["reply"] = "資料不足のため確認してください"
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil || calls != 2 || result["ready"] != false || result["reply"] != "資料不足のため確認してください" || len(fleet.Elements(result["questions"])) == 0 {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}

func TestScopeRetryUsesSavedPendingAnswer(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(reply()), nil })
	old := reply()
	old["reply"] = `{"relation":"required","evidence":"資料の成果物を利用"}`
	old["changeScope"] = Record{"known": true, "paths": []any{}}
	d["consultationBudget"] = Record{"calls": 1, "weighted": 0, "unknownCalls": 0, "contextRead": Record{"rounds": 0, "answerPending": true, "lastReply": old}}
	result, err := s.Turn(context.Background(), d)
	if err != nil || calls != 0 || result["reply"] != old["reply"] {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}

func TestFinalizeSaveFailureDoesNotRepair(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["changeScope"] = Record{"known": true, "paths": []any{}}
		return measuredAnswer(v), nil
	})
	_, err := s.options.Records.DB.ExecContext(context.Background(), `CREATE FUNCTION fail_final_context() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (NEW.json::jsonb ? 'context') AND NOT (OLD.json::jsonb ? 'context') THEN RAISE EXCEPTION 'fixture context save failed'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_final_context BEFORE UPDATE ON drafts FOR EACH ROW EXECUTE FUNCTION fail_final_context()`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Turn(context.Background(), d)
	var failure *ai.Failure
	if calls != 1 || !errors.As(err, &failure) || failure.Kind != "reply_check" || !strings.Contains(err.Error(), "fixture context save failed") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil || fleet.Object(saved["formatRepairs"]) != nil {
		t.Fatalf("record=%v err=%v", saved, err)
	}
}

func TestEstimateScopeBoundaries(t *testing.T) {
	for _, count := range []int{0, 1, 40, 41} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _ := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				paths := []any{}
				for i := 0; i < count; i++ {
					paths = append(paths, fmt.Sprintf("src/%d.go", i))
				}
				return measuredAnswer(map[string]any{"readFiles": []any{}, "reviewedFiles": []any{"README.md"}, "changeScope": Record{"known": true, "paths": paths}}), nil
			})
			got, err := s.EstimateScope(context.Background(), "owner/app", map[string]any{"body": "対象の変更"})
			if (err != nil) != (count == 0 || count == 41) || err == nil && len(fleet.Elements(got["paths"])) != count {
				t.Fatalf("count=%d scope=%v err=%v", count, got, err)
			}
		})
	}
}
