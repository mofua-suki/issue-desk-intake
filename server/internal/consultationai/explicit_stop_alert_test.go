package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
)

func TestExplicitStopAlertsThresholdAndResolution(t *testing.T) {
	s, _ := testService(t, callFunc(func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("GET invoked AI")
		return ai.Answer{}, nil
	}))
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s.options.Now = func() time.Time { return now }
	if _, err := s.options.Records.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	stop := Record{"at": now.Add(-24 * time.Hour).Format(time.RFC3339Nano), "by": "token:nas", "reason": "前提のマージを待つ"}
	d := Record{"id": id, "project": "p", "title": "停止中の相談", "status": "stalled", "stallCause": "explicit", "consultationStops": []any{stop}, "body": "private"}
	put := func() {
		t.Helper()
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.options.Records.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	read := func(want int) []any {
		t.Helper()
		got, err := s.DraftAlerts(ctx)
		if err != nil || len(got) != want {
			t.Fatalf("alerts=%v err=%v want=%d", got, err, want)
		}
		return got
	}
	put()
	got := read(1)[0].(map[string]any)
	if got["kind"] != "consultation_stop" || got["handoff"] != true || got["requiresUser"] != false || got["elapsedMinutes"] != int64(1440) || got["draftId"] != id {
		t.Fatalf("alert=%v", got)
	}
	for _, text := range []string{"前提のマージを待つ", "token:nas", "2026-11-12 21:00:00 JST", "24時間0分"} {
		if !strings.Contains(got["reason"].(string), text) {
			t.Errorf("reason missing %s: %v", text, got)
		}
	}
	now = now.Add(-time.Nanosecond)
	read(0)
	now = now.Add(time.Nanosecond)
	for _, status := range []string{"thinking", "draft", "finished", "closed", "submitted"} {
		d["status"] = status
		put()
		read(0)
	}
	d["status"] = "stalled"
	d["stallCause"] = "timeout"
	put()
	read(0)
	delete(d, "stallCause")
	d["error"] = "24時間以内にAIの応答を返せなかったため、自動処理を停止しました。"
	put()
	read(0)
	d["error"] = "相談を停止しました: 前提のマージを待つ"
	put()
	read(1)
	d["stallCause"] = "unknown"
	put()
	read(0)
	d["stallCause"] = "explicit"
	for _, at := range []string{"invalid", now.Add(time.Hour).Format(time.RFC3339Nano)} {
		stop["at"] = at
		put()
		read(0)
	}
	// The current record, not an alert cache, decides whether a removed stop remains.
	if _, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, row consultations.Record) (consultations.Record, error) {
		row["status"] = "closed"
		return row, nil
	}); err != nil {
		t.Fatal(err)
	}
	read(0)
}
