package consultations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftCacheObservation(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	writer, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &draftFailureControl{stage: "keys"}
	db := sql.OpenDB(draftFailureConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer db.Close()
	reader := &Records{DB: db}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	read := func(p DraftReadPurpose) error { _, e := reader.ListDraftEntries(ctx, p); return e }
	check := func(p DraftReadPurpose, count int) {
		t.Helper()
		var last map[string]any
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
			t.Fatal(err)
		}
		if last["purpose"] != string(p) || last["status"] != "success" || last["count"] != float64(count) || last["measured_at"] == nil {
			t.Fatalf("event: %v", last)
		}
		query, e := draftProjectionSQL(p)
		if e != nil {
			t.Fatal(e)
		}
		rows, e := writer.DB.QueryContext(ctx, query)
		if e != nil {
			t.Fatal(e)
		}
		size := 0
		for rows.Next() {
			var key, raw string
			if e = rows.Scan(&key, &raw); e != nil {
				t.Fatal(e)
			}
			size += len(key) + len(raw)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		rows.Close()
		if last["logical_bytes"] != float64(size) {
			t.Fatalf("bytes: %v want %d", last, size)
		}
	}
	purposes := []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling}
	for _, p := range purposes {
		if err = read(p); err != nil {
			t.Fatal(err)
		}
		check(p, 0)
	}
	if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('secret-key', '{"id":"secret-id","status":"draft","body":"secret-body"}')`); err != nil {
		t.Fatal(err)
	}
	for _, p := range purposes {
		if err = read(p); err != nil {
			t.Fatal(err)
		}
		check(p, 1)
	}
	before := logs.Len()
	projections := control.projections
	if err = read(DraftReadPage); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != before || control.projections != projections {
		t.Fatal("unchanged read emitted event or fetched projections")
	}
	if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"secret-id","status":"changed","body":"secret-body"}'`); err != nil {
		t.Fatal(err)
	}
	for _, p := range purposes {
		for _, failure := range []struct{ stage, mode string }{{"keys", "query"}, {"keys", "close"}, {"projection", "close"}} {
			t.Run(string(p)+"/"+failure.stage+"/"+failure.mode, func(t *testing.T) {
				// A changed row ensures that projection failures reach the projection query.
				if _, err := writer.DB.ExecContext(ctx, `UPDATE drafts SET json=json`); err != nil {
					t.Fatal(err)
				}
				control.stage, control.mode = failure.stage, failure.mode
				before := logs.Len()
				if err := read(p); err == nil {
					t.Fatal("expected failure")
				}
				var missing map[string]any
				lines := strings.Split(strings.TrimSpace(logs.String()[before:]), "\n")
				if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &missing) != nil {
					t.Fatalf("expected one missing event: %q", logs.String()[before:])
				}
				if missing["purpose"] != string(p) || missing["status"] != "missing" || missing["count"] != nil || missing["logical_bytes"] != nil || missing["measured_at"] == nil {
					t.Fatalf("missing: %v", missing)
				}
				before = logs.Len()
				control.mode = failure.mode
				if err := read(p); err == nil || logs.Len() != before {
					t.Fatal("repeated failure logged or succeeded")
				}
				if err := read(p); err != nil {
					t.Fatal(err)
				}
				if logs.Len() == before {
					t.Fatal("recovery did not emit an event")
				}
				check(p, 1)
			})
		}
	}
	if _, err = writer.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
		t.Fatal(err)
	}
	if err = read(DraftReadPage); err != nil {
		t.Fatal(err)
	}
	check(DraftReadPage, 0)
	if strings.Contains(logs.String(), "secret-") {
		t.Fatal("sensitive input leaked into log")
	}
}
