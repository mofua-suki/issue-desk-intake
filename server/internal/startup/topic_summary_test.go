package startup

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/topicdb"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTopicBackfillStartupAndRestart(t *testing.T) {
	var config *Config
	app := openLifecycleApp(t, lifecycleGitea, func(c *Config) {
		config = c
		db, err := openApplicationStore(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		_, err = db.DB().Exec(`INSERT INTO drafts(id,json) SELECT 'legacy-' || n, CASE WHEN n=1 THEN '{"context":"\u0000"}' ELSE '{"status":"draft"}' END FROM generate_series(1,1000) n`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.DB().Exec(`UPDATE drafts SET topic_summary = NULL, topic_summary_processed = false WHERE id LIKE 'legacy-%'`); err != nil {
			t.Fatal(err)
		}
	})
	response := httptest.NewRecorder()
	app.Mux.ServeHTTP(response, httptest.NewRequest("GET", "http://localhost:8787/api/openapi.json", nil))
	if response.Code != 200 && response.Code != 401 {
		t.Fatalf("API: %d %s", response.Code, response.Body.String())
	}
	app.Close()
	restarted, err := OpenApp(context.Background(), config, app.DB, "restart", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pending int
		err = restarted.SQLite.DB().QueryRow(`SELECT count(*) FROM drafts WHERE id LIKE 'legacy-%' AND NOT topic_summary_processed`).Scan(&pending)
		if err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending after restart: %d", pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var processed, fallback bool
	if err = restarted.SQLite.DB().QueryRow(`SELECT topic_summary_processed, topic_summary IS NULL FROM drafts WHERE id='legacy-1'`).Scan(&processed, &fallback); err != nil || !processed || !fallback {
		t.Fatalf("invalid: %v %v %v", processed, fallback, err)
	}
}

func TestTopicBackfillStopsScanningAfterSuccess(t *testing.T) {
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	if err := topicdb.InstallTopicSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES('legacy','{}'); UPDATE drafts SET topic_summary_processed=false`); err != nil {
		t.Fatal(err)
	}
	run := topicSummaryBackfill(env.DB, env.Schema)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := run(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backfill: %v", err)
	}
	if err := run(ctx); err != nil {
		t.Fatal(err)
	}
	var processed bool
	if err := env.DB.QueryRow(`SELECT topic_summary_processed FROM drafts WHERE id='legacy'`).Scan(&processed); err != nil || !processed {
		t.Fatalf("retry did not finish legacy row: %v %v", processed, err)
	}
	if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES('new','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.QueryRow(`SELECT topic_summary_processed FROM drafts WHERE id='new'`).Scan(&processed); err != nil || !processed {
		t.Fatalf("new write was not projected: %v %v", processed, err)
	}
	if err := env.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx); err != nil {
		t.Fatalf("completed backfill queried the database again: %v", err)
	}
}
