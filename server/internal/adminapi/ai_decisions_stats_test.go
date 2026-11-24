package adminapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

// Reference for 3b6b4652's jsonList + stable at/id sort + aggregation. Keep the
// grouping independent of the new compact-field implementation.
func referenceAIStats(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	entries, err := jsonList(ctx, tx, "ai_decisions")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := str(entries[i]["at"]), str(entries[j]["at"])
		if a == b {
			return str(entries[i]["id"]) < str(entries[j]["id"])
		}
		return a < b
	})
	groups := []map[string]any{}
	indices := map[string]int{}
	for _, d := range entries {
		kind := str(d["kind"])
		index, exists := indices[kind]
		if !exists {
			index = len(groups)
			indices[kind] = index
			groups = append(groups, map[string]any{"kind": kind, "hit": 0, "miss": 0, "partial": 0, "scored": 0, "rate": nil})
		}
		g := groups[index]
		switch d["score"] {
		case "hit", "miss", "partial":
			key := str(d["score"])
			g[key] = g[key].(int) + 1
			g["scored"] = g["scored"].(int) + 1
		}
	}
	for _, g := range groups {
		if g["scored"].(int) > 0 {
			g["rate"] = float64(g["hit"].(int)) / float64(g["scored"].(int))
		}
	}
	return groups, nil
}

// Meter the real driver's returned column values, not HTTP bytes or projected
// fake rows. UTF-8 string/[]byte lengths and textual scalar lengths are summed;
// NULL is zero. Protocol framing, query text, and server-internal bytes are not
// included. All SELECT attempts (including failures) and all delivered rows of
// both the projection and legacy re-read contribute. Exec commands are separate.
// Use a single connection and synchronous calls; the meter needs no mutex.
type statsMeter struct{ bytes, reads, commands int64 }
type statsConnector struct {
	driver.Connector
	meter *statsMeter
}

func (c statsConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &statsConn{PostgresConn: conn.(appsql.PostgresConn), meter: c.meter}, nil
}

type statsConn struct {
	appsql.PostgresConn
	meter *statsMeter
}

func (c *statsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.meter.reads++
	rows, err := c.PostgresConn.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &statsRows{Rows: rows, meter: c.meter}, nil
}
func (c *statsConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.meter.commands++
	return c.PostgresConn.ExecContext(ctx, query, args)
}

type statsRows struct {
	driver.Rows
	meter *statsMeter
}

func (r *statsRows) Next(values []driver.Value) error {
	if err := r.Rows.Next(values); err != nil {
		return err
	}
	for _, value := range values {
		switch v := value.(type) {
		case string:
			r.meter.bytes += int64(len(v))
		case []byte:
			r.meter.bytes += int64(len(v))
		case bool:
			r.meter.bytes += int64(len(strconv.FormatBool(v)))
		case int64:
			r.meter.bytes += int64(len(strconv.FormatInt(v, 10)))
		case nil:
		default:
			return fmt.Errorf("unmeasured driver value type %T", value)
		}
	}
	return nil
}

func statsDB(t *testing.T) (*sql.DB, *statsMeter) {
	t.Helper()
	env := pgtest.NewIsolated(t) // Same schema and binding connector as production.
	config, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal("cannot parse isolated test connection")
	}
	config.RuntimeParams["search_path"] = env.Schema
	meter := &statsMeter{}
	db := sql.OpenDB(statsConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), meter})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db, meter
}

func insertStatsRows(t *testing.T, db *sql.DB, raws []string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, raw := range raws {
		// Deliberately different from the document id, in reverse key order.
		if _, err := tx.Exec("INSERT INTO ai_decisions(id,json) VALUES(?,?)", fmt.Sprintf("db-%06d", len(raws)-i), raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type statsReader func(context.Context, *sql.Tx) ([]map[string]any, error)

func readStats(t *testing.T, db *sql.DB, reader statsReader) ([]map[string]any, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, readErr := reader(ctx, tx)
	// Even after JSONB SELECT fails, fallback must restore a usable transaction.
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("transaction unusable after stats: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return got, readErr
}

func TestAIStatsCompatibilityPostgres(t *testing.T) {
	cases := []struct {
		name     string
		raws     []string
		fail     bool
		fallback bool
	}{
		{"empty", nil, false, false},
		{"order-types-counts", []string{
			`{"at":"same","id":"a","kind":"stable-first","score":"hit"}`,
			`{"at":"same","id":"a","kind":"stable-second","score":"miss"}`,
			`{"at":"same","id":"B","kind":"id-before-a","score":"partial"}`,
			`{"at":"2026-01-01T00:00:00Z","kind":"time-later"}`,
			`{"at":"2026-01-01T00:00:00.1Z","kind":"lexical-earlier","score":"HIT"}`,
			`{"at":0,"id":false,"kind":"missing-time","score":null}`,
			`{"at":null,"kind":null,"score":123}`,
			`{"at":[],"id":{},"kind":[],"score":{}}`,
			`{"kind":true,"score":[]}`,
			`null`, `{}`,
			`{"at":"same","id":"z","kind":"stable-first","score":"miss"}`,
			`{"at":"same","id":"zz","kind":"stable-first","score":"partial"}`,
			`{"at":"same","id":"zzz","kind":"stable-first","score":"pending"}`,
			`{"kind":"duplicate-old","kind":"duplicate-last","score":"miss","score":"hit"}`,
		}, false, true},
		{"flat-options", []string{`{"kind":"x","options":["one","two"],"context":"text"}`}, false, false},
		{"nested-object-shallow", []string{`{"kind":"x","context":{"nested":"value"}}`}, false, true},
		{"nested-array-shallow", []string{`{"kind":"x","context":[["value"]]}`}, false, true},
		{"duplicate-deep-overwritten", []string{`{"kind":"deep","context":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `,"context":"flat"}`}, true, true},
		{"nul-context", []string{`{"kind":"x","score":"hit","context":"a\u0000b"}`}, false, true},
		{"nul-fields", []string{`{"at":"a\u0000","id":"b\u0000","kind":"k\u0000","score":"hit"}`}, false, true},
		{"nul-score", []string{`{"kind":"x","score":"hit\u0000"}`}, false, true},
		{"nul-key", []string{`{"ki\u0000nd":"ignored","kind":"x"}`}, false, true},
		{"surrogates", []string{`{"kind":"\ud800","context":"\udfff"}`}, false, true},
		{"large-flat-number", []string{`{"kind":"x","context":1e400}`}, false, false},
		{"large-flat-array-number", []string{`{"kind":"x","context":[1e400]}`}, false, false},
		{"large-number", []string{`{"kind":"x","context":1e1000000}`}, false, true},
		{"trailing-json", []string{`{"kind":"x","score":"hit"} {"kind":"ignored"}`}, false, true},
		{"trailing-garbage", []string{`{"kind":"x"} garbage`}, false, true},
		{"root-array", []string{`[]`}, true, true},
		{"root-late", []string{`{"kind":"first","score":"hit"}`, `{"kind":"second"}`, `[]`}, true, true},
		{"root-middle", []string{`{"kind":"first","score":"hit"}`, `[]`, `{"kind":"second","score":"miss"}`}, true, true},
		{"root-string", []string{`"x"`}, true, true},
		{"root-number", []string{`12`}, true, true},
		{"root-bool", []string{`false`}, true, true},
		{"malformed", []string{`{"kind":`}, true, true},
		{"empty-text", []string{``}, true, true},
		{"depth-at-go-limit", []string{`{"kind":"deep","context":` + strings.Repeat("[", 9999) + `0` + strings.Repeat("]", 9999) + `}`}, false, true},
		{"depth-over-go-limit", []string{`{"kind":"deep","context":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `}`}, true, true},
		{"raw-control", []string{"{\"context\":\"\x01\"}"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, meter := statsDB(t)
			insertStatsRows(t, db, tc.raws)
			want, refErr := readStats(t, db, referenceAIStats)
			refBytes := meter.bytes
			*meter = statsMeter{}
			got, err := readStats(t, db, aiDecisionStats)
			if (err != nil) != tc.fail || (refErr != nil) != tc.fail || !reflect.DeepEqual(got, want) {
				t.Fatalf("stats=%v err=%v; reference=%v err=%v", got, err, want, refErr)
			}
			reads := int64(2) // projection + readStats's transaction liveness SELECT
			if tc.fallback {
				reads++
			}
			if tc.name == "root-late" && meter.bytes <= refBytes {
				t.Fatal("projection bytes before fallback were excluded", *meter, refBytes)
			}
			// first/hit/true + false + second/miss/true = 31 UTF-8 bytes.
			if tc.name == "root-middle" && meter.bytes-refBytes != 31 {
				t.Fatal("projection rows after incompatible root were excluded", *meter, refBytes)
			}
			if meter.reads != reads {
				t.Fatalf("reads=%d want=%d", meter.reads, reads)
			}
			rows, readErr := db.Query(`SELECT json FROM ai_decisions ORDER BY source_rowid`)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var saved []string
			for rows.Next() {
				var raw string
				if err := rows.Scan(&raw); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				saved = append(saved, raw)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			if !reflect.DeepEqual(saved, tc.raws) {
				t.Fatalf("statistics changed original saved JSON: %v", saved)
			}
			if tc.name == "order-types-counts" {
				kinds := []string{}
				for _, g := range got {
					kinds = append(kinds, g["kind"].(string))
				}
				if strings.Join(kinds, ",") != "missing-time,,duplicate-last,lexical-earlier,time-later,id-before-a,stable-first,stable-second" {
					t.Fatal(kinds)
				}
				if g := got[6]; g["scored"] != 3 || g["hit"] != 1 || g["miss"] != 1 || g["partial"] != 1 || g["rate"] != 1.0/3.0 {
					t.Fatal(g)
				}
				if got[0]["scored"] != 0 || got[0]["rate"] != nil {
					t.Fatal(got[0])
				}
			}
		})
	}
}

func TestAIStatsHTTPContract(t *testing.T) {
	f := newFixture(t)
	for _, credential := range []string{"", "worker", "screen", "cli", "hostd"} {
		code, value := f.call(http.MethodGet, "/api/ai-decisions/stats", credential, nil)
		want := 403
		if credential == "" {
			want = 401
		}
		status(t, code, want, value)
	}
	code, value := f.call(http.MethodGet, "/api/ai-decisions/stats", "delegate", nil)
	status(t, code, 200, value)
	if !reflect.DeepEqual(value, []any{}) {
		t.Fatal(value)
	}
	insertStatsRows(t, f.db.DB(), []string{`{"kind":"a","score":"hit"}`, `{"kind":"b"}`})
	code, value = f.call(http.MethodGet, "/api/ai-decisions/stats?kind=a&who=chat&limit=1&page=99", "delegate", nil)
	status(t, code, 200, value)
	if len(value.([]any)) != 2 || obj(value.([]any)[1])["rate"] != nil {
		t.Fatal(value)
	}
	// A legacy decoding failure reaches the existing HTTP problem response.
	if _, err := f.db.DB().Exec("INSERT INTO ai_decisions(id,json) VALUES(?,?)", "invalid-root", `[]`); err != nil {
		t.Fatal(err)
	}
	code, value = f.call(http.MethodGet, "/api/ai-decisions/stats", "delegate", nil)
	status(t, code, 503, value)
}

func statsFixture(t *testing.T, count, contextChars, optionChars int, compatibility bool) []string {
	t.Helper()
	raws := make([]string, count)
	for i := range raws {
		options := make([]any, 8)
		for j := range options {
			options[j] = strings.Repeat("択", optionChars)
		}
		record, err := normalizeAI(map[string]any{
			"id": fmt.Sprintf("id-%06d", count-i), "at": "2026-11-01T00:00:00Z", "kind": fmt.Sprintf("kind-%d", i%8),
			"score": []any{"hit", "miss", "partial", nil}[i%4], "who": "chat", "pc": "test-host", "project": "owner/repo",
			"context": strings.Repeat("文", contextChars), "options": options, "chosen": "選択", "reason": "根拠", "expected": "予測", "check": "確認",
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		raws[i] = string(raw)
	}
	if compatibility {
		raws[len(raws)-1] += " {}"
	} // Original Decode accepts a second JSON document.
	return raws
}

// Five warm repetitions, same rows, transaction boundaries, database, connection
// and Go process for both readers. Timing is descriptive, never a pass threshold.
// TotalAlloc includes the real DB driver, decode, sort and grouping. No DSN gate
// beyond the repository's pgtest fixture; verify.sh provisions PostgreSQL.
func TestAIStatsTransferAndComparisonPostgres(t *testing.T) {
	const count, repetitions = 128, 5
	t.Logf("Go=%s OS=%s/%s rows=%d repeats=%d warm connection; production schema; UTF-8 column-value bytes, no wire framing", runtime.Version(), runtime.GOOS, runtime.GOARCH, count, repetitions)
	for _, compatibility := range []bool{false, true} {
		var smallBytes int64
		for _, size := range []struct{ context, option int }{{16, 8}, {16000, 2000}} {
			db, meter := statsDB(t)
			var version string
			if err := db.QueryRow("SHOW server_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			raws := statsFixture(t, count, size.context, size.option, compatibility)
			insertStatsRows(t, db, raws)
			stored := 0
			for _, raw := range raws {
				stored += len(raw)
			}
			var expected []map[string]any
			var baselineBytes int64
			for _, reader := range []struct {
				name string
				read statsReader
			}{{"reference", referenceAIStats}, {"new", aiDecisionStats}} {
				run := func() []map[string]any {
					tx, err := db.Begin()
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					got, err := reader.read(context.Background(), tx)
					if err != nil {
						t.Fatal(err)
					}
					return got
				}
				warm := run()
				if reader.name == "reference" {
					expected = warm
				} else if !reflect.DeepEqual(warm, expected) {
					t.Fatal("output changed")
				}
				*meter = statsMeter{}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				start := time.Now()
				for i := 0; i < repetitions; i++ {
					if got := run(); !reflect.DeepEqual(got, expected) {
						t.Fatal("output changed")
					}
				}
				elapsed := time.Since(start)
				runtime.ReadMemStats(&after)
				t.Logf("PG=%s compatibility=%t context=%d option=%dx8 stored=%d reader=%s bytes/op=%d reads/op=%d commands/op=%d time/op=%s allocatedBytes/op=%d allocs/op=%d", version, compatibility, size.context, size.option, stored, reader.name, meter.bytes/repetitions, meter.reads/repetitions, meter.commands/repetitions, elapsed/repetitions, (after.TotalAlloc-before.TotalAlloc)/repetitions, (after.Mallocs-before.Mallocs)/repetitions)
				if reader.name == "reference" {
					baselineBytes = meter.bytes / repetitions
					if baselineBytes != int64(stored) || meter.reads != repetitions {
						t.Fatal("reference boundary changed", *meter, stored)
					}
					continue
				}
				wantReads := int64(1)
				if compatibility {
					wantReads = 2
				}
				if meter.reads != wantReads*repetitions {
					t.Fatal("missing or extra reads", *meter)
				}
				bytes := meter.bytes / repetitions
				if compatibility {
					if bytes < baselineBytes {
						t.Fatal("fallback read bytes excluded")
					}
				} else if size.context == 16 {
					smallBytes = bytes
				} else if bytes != smallBytes || bytes >= baselineBytes/10 {
					t.Fatalf("payload-dependent projection: small=%d large=%d full=%d", smallBytes, bytes, baselineBytes)
				}
			}
		}
	}
}

var _ driver.Rows = (*statsRows)(nil)
