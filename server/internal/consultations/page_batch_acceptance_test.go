package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

type pageReadSample struct{ calls, rows, bytes int }
type pageReadLog struct {
	full, fallback, projection, projectionFallback, other pageReadSample
	beforeQuery                                           func(int)
}
type pageCountConnector struct {
	driver.Connector
	log *pageReadLog
}

func (c pageCountConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pageCountConn{PostgresConn: conn.(appsql.PostgresConn), raw: conn.(*stdlib.Conn), log: c.log}, nil
}

type pageCountConn struct {
	appsql.PostgresConn
	raw *stdlib.Conn
	log *pageReadLog
}

func (c *pageCountConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	bound, err := appsql.BindPostgresQuery(query)
	if err != nil {
		return nil, err
	}
	// Describe on the same connection, before the execution, preserves table
	// OID and attribute number even when the expression is also named json.
	desc, err := c.raw.Conn().Prepare(ctx, "", bound)
	if err != nil {
		return nil, err
	}
	kind := 0
	for _, field := range desc.Fields {
		if !(field.TableOID == 0 && field.Name == "json" || field.TableOID != 0 && field.TableAttributeNumber == 3) {
			continue
		}
		if field.TableOID == 0 {
			kind = 1
		} else if len(args) == 0 {
			kind = 3
		} else {
			kind = 2
		}
	}
	if c.log.beforeQuery != nil {
		c.log.beforeQuery(kind)
	}
	rows, err := c.PostgresConn.QueryContext(ctx, bound, args)
	if err != nil {
		return nil, err
	}
	return &pageCountRows{Rows: rows, log: c.log, kind: kind}, nil
}

type pageCountRows struct {
	driver.Rows
	log                         *pageReadLog
	kind                        int
	rows, bytes                 int
	fallbackRows, fallbackBytes int
}

func (r *pageCountRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil {
		r.rows++
		fallback := false
		if r.kind == 1 {
			for _, value := range values {
				var raw string
				switch v := value.(type) {
				case string:
					raw = v
				case []byte:
					raw = string(v)
				}
				if strings.Contains(raw, `\u0000`) {
					fallback = true
				}
			}
		}
		if fallback {
			r.fallbackRows++
		}
		for _, value := range values {
			var s string
			switch v := value.(type) {
			case string:
				s = v
			case []byte:
				s = string(v)
			}
			r.bytes += len(s)
			if fallback {
				r.fallbackBytes += len(s)
			}
		}
	}
	if err == io.EOF {
		r.finish()
	}
	return err
}
func (r *pageCountRows) finish() {
	if r.kind < 0 {
		return
	}
	var sample *pageReadSample
	switch r.kind {
	case 2:
		sample = &r.log.full
	case 3:
		sample = &r.log.fallback
	case 1:
		sample = &r.log.projection
	default:
		sample = &r.log.other
	}
	if r.fallbackRows > 0 {
		r.log.projectionFallback.calls++
		r.log.projectionFallback.rows += r.fallbackRows
		r.log.projectionFallback.bytes += r.fallbackBytes
	}
	sample.calls++
	sample.rows += r.rows
	sample.bytes += r.bytes
	r.kind = -1
}
func (r *pageCountRows) Close() error { r.finish(); return r.Rows.Close() }

// PostgreSQL column origin distinguishes stored json from a projection even
// when both columns have the same name or the result has no rows.
func TestPageBatchActualPostgresReads(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key-%03d", i)
		raw := fmt.Sprintf(`{"id":"different-%03d","project":"owner/app","status":"draft","body":"body-%03d","familyProgress":{"stale":true}}`, i, i)
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	log := &pageReadLog{}
	readDB := sql.OpenDB(pageCountConnector{stdlib.GetConnector(*cfg), log})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	token := stopToken(t, db, true)
	// Obtain the actual database ordering independently of the JSON id.
	entries, err := (&Records{DB: env.DB}).ListDraftEntries(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	for _, tc := range []struct {
		name, query string
		n           int
	}{{"one", "?limit=1", 1}, {"default", "", 100}, {"maximum", "?limit=200", 200}, {"empty", "?limit=1&page=999", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			*log = pageReadLog{}
			req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			var out struct {
				Items   []map[string]any `json:"items"`
				Total   int              `json:"total"`
				HasMore bool             `json:"hasMore"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body.String(), err)
			}
			wantCalls := 1
			if tc.n == 0 {
				wantCalls = 0
			}
			if log.full.calls != wantCalls || log.full.rows != tc.n || log.fallback.calls != 0 || len(out.Items) != tc.n || out.Total != 200 || out.HasMore != (tc.n > 0 && tc.n < 200) {
				t.Fatalf("response=%+v reads=%+v", out, log)
			}
			for i, item := range out.Items {
				suffix := strings.TrimPrefix(entries[i].Key, "key-")
				if item["id"] != "different-"+suffix || item["body"] != "body-"+suffix {
					t.Fatalf("item %d key=%s item=%v", i, entries[i].Key, item)
				}
				if _, ok := item["familyProgress"]; ok {
					t.Fatalf("stale familyProgress retained: %v", item)
				}
			}
			t.Logf("actual reads: full=%+v projection=%+v other=%+v", log.full, log.projection, log.other)
		})
	}
}

func TestPageReadCounterPostgresOrigins(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('a','{"id":"a","status":"draft"}')`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	log := &pageReadLog{}
	db := sql.OpenDB(pageCountConnector{stdlib.GetConnector(*cfg), log})
	defer db.Close()
	r := &Records{DB: db}
	entries, err := r.ListDraftEntries(ctx, DraftReadPage)
	if err != nil || len(entries) != 1 || log.projection.calls != 1 || log.projection.rows != 1 || log.fallback.calls != 0 {
		t.Fatalf("projection=%+v entries=%v err=%v", log, entries, err)
	}
	*log = pageReadLog{}
	got, err := r.GetDraftEntries(ctx, []string{"a"})
	if err != nil || len(got) != 1 || got[0].Record["body"] != nil || log.full.calls != 1 || log.full.rows != 1 || log.full.bytes == 0 {
		t.Fatalf("bodyless=%+v entries=%v err=%v", log, got, err)
	}
	*log = pageReadLog{}
	got, err = r.GetDraftEntries(ctx, []string{"missing"})
	if err != nil || len(got) != 0 || log.full.calls != 1 || log.full.rows != 0 {
		t.Fatalf("missing=%+v entries=%v err=%v", log, got, err)
	}
	const exceptional = `{"id":"z","status":"draft","extra":"before\u0000after"}`
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, "z", exceptional); err != nil {
		t.Fatal(err)
	}
	*log = pageReadLog{}
	entries, err = (&Records{DB: db}).ListDraftEntries(ctx, DraftReadPage)
	if err != nil || len(entries) != 2 || log.projection.calls != 1 || log.projectionFallback.calls != 1 || log.projectionFallback.rows != 1 || log.projectionFallback.bytes != len("z")+len(exceptional) || entries[0].Record["extra"] != "before\x00after" {
		t.Fatalf("projection fallback=%+v entries=%v err=%v", log, entries, err)
	}
	t.Logf("projection fallback: %+v", log.projectionFallback)
}

func TestPageFamilyDoesNotReadOutsidePage(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, raw string }{
		{"child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"parent","issue":{"number":7},"body":"child body"}`},
		{"parent", `{"id":"parent","project":"owner/app","status":"draft","body":"parent body","splitChildren":["child"],"splitIssues":[{}]}`},
	} {
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	log := &pageReadLog{}
	readDB := sql.OpenDB(pageCountConnector{stdlib.GetConnector(*cfg), log})
	defer readDB.Close()
	s, authDB, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	token := stopToken(t, authDB, true)
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?status=draft&limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Items) != 1 || out.Items[0]["id"] != "parent" {
		t.Fatalf("status=%d response=%s err=%v", w.Code, w.Body.String(), err)
	}
	if log.full.calls != 1 || log.full.rows != 1 || log.fallback.calls != 0 {
		t.Fatalf("page selected one parent but full queries include page-external family: full=%+v projection=%+v fallback=%+v other=%+v", log.full, log.projection, log.fallback, log.other)
	}
	t.Logf("family page reads: full=%+v projection=%+v fallback=%+v other=%+v", log.full, log.projection, log.fallback, log.other)
}
