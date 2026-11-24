package statistics

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestStopChecksReviewDatesAndOrdering(t *testing.T) {
	var ledger []stopCheckLedgerEntry
	if err := json.Unmarshal([]byte(`[{"id":"today","nextReviewAt":"2026-12-05","reviews":[{"date":"2026-12-02","result":"直す"},{"date":"2026-11-01","result":"残す"}]},{"id":"past","nextReviewAt":"2026-12-04"},{"id":"future","nextReviewAt":"2026-12-06"}]`), &ledger); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 12, 4, 15, 0, 0, 0, time.UTC)
	rows := buildStopChecks(ledger, nil, now)["rows"].([]stopCheckRow)
	if rows[0].ID != "past" || !rows[0].Overdue || rows[1].ID != "today" || rows[1].Overdue || rows[2].Overdue {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[1].ReviewResult == nil || *rows[1].ReviewResult != "直す" || rows[0].ReviewResult != nil {
		t.Fatalf("reviews: %+v", rows)
	}
}

func TestPostgresStopChecksReport(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	records := []StopCheckRecord{
		{ID: "123e4567-e89b-12d3-a456-426614174001", CheckID: "unasked-send", Result: "block", OccurredAt: now.Add(-31 * 24 * time.Hour).Format(time.RFC3339), Project: "p", Host: "pc"},
		{ID: "123e4567-e89b-12d3-a456-426614174002", CheckID: "unasked-send", Result: "warn", OccurredAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339), Project: "p", Host: "pc"},
		{ID: "123e4567-e89b-12d3-a456-426614174003", CheckID: "unasked-send", Result: "block", OccurredAt: now.Format(time.RFC3339), Project: "p", Host: "pc"},
	}
	for _, record := range records {
		if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	// Insert anomalies and both sides of the inclusive 30-day window directly.
	for _, item := range []struct {
		id, check string
		at        time.Time
	}{
		{"123e4567-e89b-12d3-a456-426614174005", "duplicate-issue-guard", now},
		{"123e4567-e89b-12d3-a456-426614174006", "unasked-send", now.Add(-30*24*time.Hour - time.Second)},
		{"123e4567-e89b-12d3-a456-426614174007", "unasked-send", now.Add(time.Second)},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO stop_check_records (id,check_id,result,occurred_at,project,host) VALUES ($1,$2,'warn',$3,'p','pc')`, item.id, item.check, item.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO stop_check_records (id,check_id,result,occurred_at,project,host) VALUES ('123e4567-e89b-12d3-a456-426614174004','retired-check','warn',$1,'p','pc')`, now); err != nil {
		t.Fatal(err)
	}
	s := Service{DB: db, Now: func() time.Time { return now }}
	value, err := s.Saved(ctx, "stopChecks", Query{})
	if err != nil {
		t.Fatal(err)
	}
	report := value.(map[string]any)
	rows := report["rows"].([]stopCheckRow)
	if !reflect.DeepEqual(report["unknownCheckIds"], []string{"retired-check"}) {
		t.Fatal(report)
	}
	var hook, guard *stopCheckRow
	for i := range rows {
		switch rows[i].ID {
		case "unasked-send":
			hook = &rows[i]
		case "duplicate-issue-guard":
			guard = &rows[i]
		}
	}
	if hook == nil || hook.Purpose == "" || hook.IntroducedAt != "2026-11-05" || hook.NextReviewAt != "2026-12-05" || hook.Overdue || hook.ReviewResult != nil || *hook.Total != 5 || *hook.Last30Days != 2 || *hook.Blocked != 2 || *hook.Warned != 3 || hook.LastFiredAt == nil || !hook.LastFiredAt.Equal(now.Add(time.Second)) {
		t.Fatalf("hook: %+v", hook)
	}
	if guard == nil || guard.Kind != "guard" || guard.Total != nil || guard.Last30Days != nil || guard.Blocked != nil || guard.Warned != nil || guard.LastFiredAt != nil {
		t.Fatalf("guard: %+v", guard)
	}
	s.Now = func() time.Time { return now.Add(24 * time.Hour) }
	value, err = s.StopChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !value.(map[string]any)["rows"].([]stopCheckRow)[0].Overdue {
		t.Fatal("expected overdue")
	}
}

// The embedded bytes can no longer be parsed after initialization; both report
// requests must still use the same startup ledger as record validation.
func TestPostgresStopChecksUsesLoadedLedger(t *testing.T) {
	db, ctx := testPostgres(t)
	original := append([]byte(nil), stopChecksJSON...)
	defer copy(stopChecksJSON, original)
	for i := range stopChecksJSON {
		stopChecksJSON[i] = 'x'
	}
	s := Service{DB: db}
	for i := 0; i < 2; i++ {
		value, err := s.StopChecks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows := value.(map[string]any)["rows"].([]stopCheckRow)
		found := false
		for _, row := range rows {
			if row.ID == "unasked-send" && row.Purpose != "" {
				found = true
			}
		}
		if !found {
			t.Fatal("embedded ledger missing from report")
		}
	}
}
