package topicdb_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/topicdb"
)

func TestRestoreAndUpdateTopicSummary(t *testing.T) {
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	_, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('a','{"topicLinks":[{"id":"b"}]}')`)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a restored database from before the projection was introduced.
	if _, err = env.DB.ExecContext(ctx, `ALTER TABLE drafts DROP COLUMN topic_summary, DROP COLUMN topic_summary_processed`); err != nil {
		t.Fatal(err)
	}
	if err = topicdb.InstallTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	if err = topicdb.BackfillTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	var summary string
	if err = env.DB.QueryRowContext(ctx, `SELECT topic_summary FROM drafts WHERE id='a'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded["topicLinks"], []any{map[string]any{"id": "b"}}) {
		t.Fatalf("restored summary: %s", summary)
	}
	if _, err = env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"status":"submitted"}' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err = env.DB.QueryRowContext(ctx, `SELECT topic_summary FROM drafts WHERE id='a'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["status"] != "submitted" {
		t.Fatalf("updated summary: %s", summary)
	}
}

func TestBackfillAllowsConcurrentWritesAndResumes(t *testing.T) {
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	for i := 0; i < 101; i++ {
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ($1, '{"status":"submitted"}')`, fmt.Sprintf("%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('invalid', '{"context":"\u0000"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET topic_summary = NULL, topic_summary_processed = false`); err != nil {
		t.Fatal(err)
	}
	tx, err := env.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT id FROM drafts WHERE id='100' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- topicdb.BackfillTopicSummary(runCtx, env.DB, env.Schema) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var ready bool
		if err := env.DB.QueryRowContext(ctx, `SELECT topic_summary IS NOT NULL FROM drafts WHERE id='000'`).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		var blocked bool
		if err := env.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1)`, "%WITH batch AS (%"+env.Schema+"%").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if ready && blocked {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("backfill did not reach locked batch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if _, err := env.DB.ExecContext(writeCtx, `UPDATE drafts SET json='{"status":"changed"}' WHERE id='000'`); err != nil {
		cancel()
		t.Fatalf("concurrent write blocked: %v", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected interrupted backfill")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := topicdb.InstallTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	if err := topicdb.BackfillTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := env.DB.QueryRowContext(ctx, `SELECT count(*) FROM drafts WHERE NOT topic_summary_processed`).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("remaining=%d err=%v", missing, err)
	}
	var invalidProcessed, fallback bool
	if err := env.DB.QueryRowContext(ctx, `SELECT topic_summary_processed, topic_summary IS NULL FROM drafts WHERE id='invalid'`).Scan(&invalidProcessed, &fallback); err != nil || !invalidProcessed || !fallback {
		t.Fatalf("invalid row processed=%v fallback=%v err=%v", invalidProcessed, fallback, err)
	}
	var summary string
	if err := env.DB.QueryRowContext(ctx, `SELECT topic_summary FROM drafts WHERE id='000'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != `{"status" : "changed"}` {
		var v map[string]any
		if json.Unmarshal([]byte(summary), &v) != nil || v["status"] != "changed" {
			t.Fatalf("concurrent update lost: %s", summary)
		}
	}
}

func TestLegacyRestoreAllowsReadsAndWritesDuringBackfill(t *testing.T) {
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	if _, err := env.DB.ExecContext(ctx, `DROP TRIGGER drafts_topic_summary ON drafts; ALTER TABLE drafts DROP COLUMN topic_summary, DROP COLUMN topic_summary_processed`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) SELECT 'old-' || lpad(n::text, 6, '0'), '{"status":"draft"}' FROM generate_series(1,20000) n`); err != nil {
		t.Fatal(err)
	}
	if err := topicdb.InstallTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	tx, err := env.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM drafts WHERE id='old-000100' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- topicdb.BackfillTopicSummary(ctx, env.DB, env.Schema) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var blocked bool
		if err := env.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1)`, "%WITH batch AS (%"+env.Schema+"%").Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backfill did not reach locked row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := env.DB.ExecContext(writeCtx, `UPDATE drafts SET json='{"status":"changed"}' WHERE id='old-019999'`); err != nil {
		t.Fatal(err)
	}
	var summary string
	if err := env.DB.QueryRowContext(writeCtx, `SELECT topic_summary FROM drafts WHERE id='old-019999'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(summary)) {
		t.Fatalf("invalid summary: %s", summary)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := env.DB.QueryRowContext(ctx, `SELECT count(*) FROM drafts WHERE NOT topic_summary_processed`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}
