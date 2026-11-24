package startup

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

func TestRecordUsageFirstOutputPostgres(t *testing.T) {
	db := appPostgres(t)
	value, zero := int64(123), int64(0)
	for i, v := range []*int64{&value, &zero, nil} {
		req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Step: usage.DraftStep, Attempt: 1, Turn: i + 1}
		if err := (&App{DB: db}).recordUsage(t.Context(), req, ai.Usage{FirstOutputMs: v}, time.Second, true); err != nil {
			t.Fatal(err)
		}
	}
	rows, skipped, err := (&usage.Store{DB: db}).Read(t.Context(), "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || skipped != 0 || len(rows) != 3 {
		t.Fatalf("rows=%d skipped=%d err=%v", len(rows), skipped, err)
	}
	for _, row := range rows {
		turn := int(row["turn"].(float64))
		got, exists := row["firstOutputMs"]
		switch turn {
		case 1:
			if !exists || got != float64(123) {
				t.Fatalf("row: %#v", row)
			}
		case 2:
			if !exists || got != float64(0) {
				t.Fatalf("row: %#v", row)
			}
		case 3:
			if exists {
				t.Fatalf("row: %#v", row)
			}
		default:
			t.Fatalf("unexpected row: %#v", row)
		}
	}
}

func TestReviewNotesFirstOutputPostgres(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   any
		exists bool
	}{
		{"measured", 123 * time.Millisecond, float64(123), true},
		{"zero", 0, float64(0), true},
		{"missing", -1, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := reviewNotesApp(t)
			app.Runner.Execute = func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
				if p.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				writeReviewNotesAnswer(t, p, `{"assignments":[]}`)
				return ai.ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":2,"cached_input_tokens":0}}`), FirstOutput: tc.offset}, nil
			}
			if _, err := app.answerReviewNotes(t.Context(), "mofua-suki/issue-desk-intake", "分類対象"); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := app.DB.QueryRow(`SELECT json FROM usage_rows WHERE json::jsonb->>'step'=$1`, usage.ReviewNotesStep).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			got, exists := row["firstOutputMs"]
			if exists != tc.exists || got != tc.want {
				t.Fatalf("saved: %s", raw)
			}
		})
	}
}
