package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestBudgetContinueDeduplicatesAcrossLaterStops(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := Record{"id": todoID, "project": "owner/app", "status": "draft", "consultationBudget": Record{"calls": 40, "weighted": 6000000, "stopped": Record{"at": "now"}, "stops": []any{Record{"at": "now"}}}}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	data := Record{"operationId": "continue-1", "stopCount": 1}
	if _, err := s.ContinueBudget(ctx, todoID, data, auth.Principal{Kind: auth.CLI}); err == nil {
		t.Fatal("CLIによる続行を許可した")
	}
	saved, err := s.ContinueBudget(ctx, todoID, data, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if saved["status"] != "thinking" || record(record(saved["consultationBudget"])["added"])["calls"] == nil {
		t.Fatal(saved)
	}
	_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "draft"
		b := record(d["consultationBudget"])
		b["stopped"] = Record{"at": "later"}
		b["stops"] = append(fleet.Items(b["stops"]), Record{"at": "later"})
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ContinueBudget(ctx, todoID, data, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if replay["status"] != "draft" || len(fleet.Items(record(replay["consultationBudget"])["continuations"])) != 1 {
		t.Fatal("同じoperationで再加算した", replay)
	}
}
func TestProxyUndoRestoresSnapshotAndInvalidatesLateRun(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := Record{"id": todoID, "project": "owner/app", "status": "thinking", "error": "古いエラー", "errorCode": "old", "run": "active", "revision": 2, "messages": []any{Record{"id": "u", "role": "user", "text": "元の依頼"}, Record{"id": "p", "role": "proxy", "advanced": true}}, "questions": []any{}}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.SaveProxy(ctx, todoID, "active", Record{}, []Record{{"id": sendID, "project": "owner/app", "draft": todoID, "messageId": "p", "at": "now", "snapshot": Record{"messageCount": 1, "questions": []any{"どちらか"}, "title": "元の題名", "body": "元の本文"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.UndoProxy(ctx, todoID, Record{"messageId": "p"}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "intake_waiting" || out["run"] != nil || len(fleet.Items(out["messages"])) != 1 || out["title"] != "元の題名" {
		t.Fatal(out)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || saved["error"] != nil || saved["errorCode"] != nil {
		t.Fatal(saved, err)
	}
	var status string
	if err := s.options.Records.DB.QueryRow("SELECT status FROM proxy_answers WHERE id=?", sendID).Scan(&status); err != nil || status != "undone" {
		t.Fatal(status, err)
	}
	_, err = s.options.Records.SaveProxy(ctx, todoID, "active", Record{"ready": true}, nil)
	if err == nil {
		t.Fatal("取消後の遅い結果を書いた")
	}
}
func TestErrorStallCountsExceptionsExcludesAIFailuresAndResolves(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.options.Now = func() time.Time { return now }
	stalls, err := NewErrorStalls(ctx, s, "revision", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := Record{"id": todoID, "project": "owner/app", "status": "error"}
	_, err = s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := stalls.Failed(ctx, d, &ai.Failure{Kind: "authentication", Message: "認証"}); err != nil {
		t.Fatal(err)
	}
	rows, err := stalls.rows(ctx, "intake_error_failures")
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if err := stalls.Failed(ctx, d, errors.New("controller fault 123")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if err := stalls.Failed(ctx, d, errors.New("controller fault 456")); err != nil {
		t.Fatal(err)
	}
	open, err := stalls.open(ctx)
	if err != nil || open == nil || !strings.Contains(text(open["gist"]), "#") {
		t.Fatal(open, err)
	}
	if err := stalls.Succeeded(ctx, d); err != nil {
		t.Fatal(err)
	}
	alerts, err := stalls.Alerts(ctx)
	if err != nil || len(alerts) != 1 || record(alerts[0])["status"] != "intake_error_fixed" {
		t.Fatal(alerts, err)
	}
}
func TestCredentialMaskForDecisionRecords(t *testing.T) {
	for _, secret := range []string{"Bearer abcdefgh12345678", "token: private-value", strings.Repeat("a", 40), "https://name:password@example.invalid"} {
		if got := MaskCredentials(secret); got == secret {
			t.Fatalf("秘密を伏せなかった: %q", got)
		}
	}
}

func TestFailureSequenceAndReplyOrder(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	stalls, err := NewErrorStalls(ctx, s, "revision", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stalls.checking = true
	for _, id := range []string{"first", "second"} {
		if err := stalls.Failed(ctx, Record{"id": id}, errors.New("failure")); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := stalls.rows(ctx, "intake_error_failures")
	if err != nil || len(rows) != 2 || rows[0]["draft"] != "first" || rows[1]["draft"] != "second" {
		t.Fatalf("failure order: %v %v", rows, err)
	}
	if _, err := s.options.Records.DB.ExecContext(ctx, "DELETE FROM intake_error_failures WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	if err := stalls.Failed(ctx, Record{"id": "third"}, errors.New("failure")); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT MAX(id) FROM intake_error_failures").Scan(&id); err != nil || id != 3 {
		t.Fatalf("sequence: %d %v", id, err)
	}
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := Record{"id": "draft", "project": "p", "questions": []any{"token: private-value"}}
	for _, m := range []Record{{"id": "one", "at": "same", "text": "Bearer abcdefgh12345678"}, {"id": "two", "at": "same", "text": "second"}, {"id": "one", "at": "same", "text": "changed"}} {
		if err := recordReply(ctx, tx, d, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordReply(ctx, tx, Record{"id": "other", "project": "other"}, Record{"id": "other", "at": "same", "text": "other"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	entries, err := s.options.Records.ProxyDecisions(ctx, "p")
	if err != nil || len(entries) != 2 || record(entries[0])["id"] != "two" || record(entries[1])["id"] != "one" {
		t.Fatalf("reply order: %v %v", entries, err)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "abcdefgh12345678") || strings.Contains(string(raw), "private-value") || strings.Contains(string(raw), "changed") {
		t.Fatalf("unmasked or overwritten: %s", raw)
	}
}
