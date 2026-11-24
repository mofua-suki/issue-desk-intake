package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftCacheJSONCompatibility(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	writer, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct{ id, raw string }{
		{"missing", `{"status":"draft","project":1e3}`},
		{"dup1", `{"id":"same","status":"draft","project":9007199254740993}`},
		{"dup2", `{"id":"same","status":"draft"}`},
		{"typed", `{"id":42,"status":"draft"}`},
		{"duplicate-key", `{"id":"first","id":"last","status":"draft"}`},
		{"nul", `{"id":"nul","status":"draft","title":"a\u0000b","body":"full\u0000body"}`},
	}
	for _, row := range fixtures {
		if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	counts := &readCounts{}
	db := sql.OpenDB(countConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), counts})
	defer db.Close()
	reader := &Records{DB: db}
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen} {
		t.Run(string(purpose), func(t *testing.T) {
			query, err := draftProjectionSQL(purpose)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := writer.readDraftEntries(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				*counts = readCounts{}
				got, err := reader.ListDraftEntries(ctx, purpose)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("projection pass %d: %#v != %#v", pass, got, baseline)
				}
				if pass == 1 && (counts.calls != 1 || counts.rows != len(fixtures)) {
					t.Fatalf("unchanged: %+v", counts)
				}
			}
			byKey := map[string]Record{}
			for _, entry := range baseline {
				byKey[entry.Key] = entry.Record
			}
			if _, ok := byKey["missing"]["id"]; ok {
				t.Fatal("missing id added")
			}
			if byKey["dup1"]["id"] != "same" || byKey["dup2"]["id"] != "same" || byKey["typed"]["id"] != json.Number("42") || byKey["duplicate-key"]["id"] != "last" {
				t.Fatalf("IDs: %#v", byKey)
			}
			if byKey["missing"]["project"] != json.Number("1e3") || byKey["dup1"]["project"] != json.Number("9007199254740993") {
				t.Fatalf("numbers: %#v", byKey)
			}
			if byKey["nul"]["body"] != "full\x00body" {
				t.Fatal("NUL fallback did not preserve full row")
			}
			// PostgreSQL's jsonb conversion rejects a lone surrogate that Go accepts.
			if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='typed'`, `{"id":42,"status":"draft","title":"\ud800","body":"must remain full"}`); err != nil {
				t.Fatal(err)
			}
			full, err := writer.readDraftEntries(ctx, "SELECT id, json FROM drafts ORDER BY "+rowOrder(true))
			if err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				*counts = readCounts{}
				got, err := reader.ListDraftEntries(ctx, purpose)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, full) {
					t.Fatalf("fallback pass %d differs", pass)
				}
				if counts.calls != 3 {
					t.Fatalf("fallback did not read whole list: %+v", counts)
				}
			}
			for _, invalid := range []string{`null`, `[]`, `42`, `{`} {
				if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='typed'`, invalid); err != nil {
					t.Fatal(err)
				}
				_, fullErr := writer.readDraftEntries(ctx, "SELECT id, json FROM drafts ORDER BY "+rowOrder(true))
				if fullErr == nil {
					t.Fatalf("full accepted %s", invalid)
				}
				for pass := 0; pass < 2; pass++ {
					if _, err = reader.ListDraftEntries(ctx, purpose); err == nil || err.Error() != fullErr.Error() {
						t.Fatalf("%s pass %d: got %v, full read %v", invalid, pass, err, fullErr)
					}
				}
			}
			if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='typed'`, fixtures[3].raw); err != nil {
				t.Fatal(err)
			}
			got, err := reader.ListDraftEntries(ctx, purpose)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err = writer.readDraftEntries(ctx, query)
			if err != nil || !reflect.DeepEqual(got, baseline) {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}
