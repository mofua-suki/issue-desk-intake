package consultationai

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftAlertsProjectedCompatibility(t *testing.T) {
	s, _ := testService(t, callFunc(func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("unexpected AI call")
		return ai.Answer{}, nil
	}))
	ctx := context.Background()
	db := s.options.Records.DB
	if _, err := db.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
		t.Fatal(err)
	}
	rows := []string{
		`{"id":"11111111-1111-4111-8111-111111111111","project":"p","title":"title","updatedAt":"now","status":"draft","parentSplit":"p","splitScopeHeld":true,"consultationBudget":{"stopped":{"calls":1e3,"weighted":9007199254740993,"limits":{"calls":2.00,"weighted":1e7},"questions":["q",null,42],"reply":"reply","at":"then"},"stops":[{},{}]},"body":"large"}`,
		`{"id":"missing","status":"draft","parentSplit":{},"splitScopeHeld":"yes","consultationBudget":{"stopped":null}}`,
		`{"id":"empty","status":null,"parentSplit":null,"splitScopeHeld":false,"consultationBudget":"wrong"}`,
		`{"id":"no-alert","status":"closed","parentSplit":"p","splitScopeHeld":true}`,
		`{"id":"bad-types","project":12,"title":null,"consultationBudget":{"stopped":{"calls":"x","limits":false,"questions":{},"reply":null}}}`,
	}
	for i, raw := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES(?,?)`, string(rune('a'+i)), raw); err != nil {
			t.Fatal(err)
		}
	}
	config := map[string]any{"intake": map[string]any{"publicUrl": "https://desk.example.invalid/"}}
	s.options.Config = func(context.Context) (map[string]any, error) { return config, nil }
	compare := func() []any {
		t.Helper()
		full, err := s.options.Records.List(ctx, "drafts")
		if err != nil {
			t.Fatal(err)
		}
		want := draftAlertsFromRecords(full, config)
		got, err := s.DraftAlerts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("full=%s projected=%s", a, b)
		}
		return got
	}
	compare()
	compare()
	if _, err := db.ExecContext(ctx, `UPDATE drafts SET json=? WHERE id=?`, `{"id":"updated","status":"draft","parentSplit":"p","splitScopeHeld":true}`, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM drafts WHERE id=?`, "b"); err != nil {
		t.Fatal(err)
	}
	compare()
	if _, err := db.ExecContext(ctx, `UPDATE drafts SET json=? WHERE id=?`, `{`, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DraftAlerts(ctx); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

type alertCounts struct {
	queries               []string
	rows                  int
	failState             string
	failOnNext            bool
	projectionErrors      []error
	projectionErrorPhases []string
	projectionRows        int
}
type alertConnector struct {
	base   driver.Connector
	counts *alertCounts
}

func (c alertConnector) Driver() driver.Driver { return c.base.Driver() }
func (c alertConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &alertConn{PostgresConn: conn.(appsql.PostgresConn), counts: c.counts}, nil
}

type alertConn struct {
	appsql.PostgresConn
	counts *alertCounts
}

func (c *alertConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.counts.queries = append(c.counts.queries, q)
	if c.counts.failState != "" && !c.counts.failOnNext && isAlertProjection(q) {
		return nil, &pgconn.PgError{Code: c.counts.failState}
	}
	r, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		if isAlertProjection(q) {
			c.counts.projectionErrors = append(c.counts.projectionErrors, err)
			c.counts.projectionErrorPhases = append(c.counts.projectionErrorPhases, "query")
		}
		return nil, err
	}
	return &alertRows{Rows: r, counts: c.counts, failNext: c.counts.failOnNext && isAlertProjection(q), projection: isAlertProjection(q)}, nil
}

type alertRows struct {
	driver.Rows
	counts     *alertCounts
	failNext   bool
	readOne    bool
	projection bool
}

func (r *alertRows) Next(v []driver.Value) error {
	if r.failNext && r.readOne {
		r.failNext = false
		return &pgconn.PgError{Code: r.counts.failState}
	}
	err := r.Rows.Next(v)
	if err != nil && err != io.EOF && r.projection {
		r.counts.projectionErrors = append(r.counts.projectionErrors, err)
		r.counts.projectionErrorPhases = append(r.counts.projectionErrorPhases, "next")
	}
	if err == nil {
		r.readOne = true
		if r.projection {
			r.counts.projectionRows++
		}
		r.counts.rows++

	}
	return err
}

func TestDraftAlertsReadCacheAndFailures(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := consultations.OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('a','{"id":"a","body":"large"}'),('b','{"id":"b"}')`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	counts := &alertCounts{}
	db := sql.OpenDB(alertConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), counts})
	defer db.Close()
	s := &Service{options: Options{Records: &consultations.Records{DB: db}, Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }}}
	read := func() {
		t.Helper()
		if _, err := s.DraftAlerts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if len(counts.queries) != 2 || counts.rows != 4 || !strings.Contains(counts.queries[1], "json_each") {
		t.Fatalf("cold: %+v", counts)
	}
	*counts = alertCounts{}
	read()
	if len(counts.queries) != 1 || counts.rows != 2 || strings.Contains(counts.queries[0], "json") {
		t.Fatalf("warm: %+v", counts)
	}
	if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"a","status":"draft","parentSplit":"p","splitScopeHeld":true}' WHERE id='a'; DELETE FROM drafts WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	*counts = alertCounts{}
	got, err := s.DraftAlerts(ctx)
	if err != nil || len(got) != 1 || len(counts.queries) != 2 || counts.rows != 2 {
		t.Fatalf("updated: %v %v %+v", got, err, counts)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DraftAlerts(ctx); err == nil {
		t.Fatal("read failure accepted")
	}
}

func isAlertProjection(query string) bool {
	return strings.Contains(query, "json_each")
}

func assertFallbackQueries(t *testing.T, queries []string) {
	t.Helper()
	if len(queries) != 3 {
		t.Fatalf("expected version, projection, one full read: %v", queries)
	}
	if strings.Contains(queries[0], "json") || !isAlertProjection(queries[1]) || !strings.Contains(queries[2], "SELECT id, json FROM drafts") {
		t.Fatalf("unexpected fallback query sequence: %v", queries)
	}
}

func TestDraftAlertsCompatibilityFallbackReadCounts(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('a','{"id":"a","status":"draft","parentSplit":"p","splitScopeHeld":true,"body":"full"}'),('b','{"id":"b","consultationBudget":{"stopped":{"calls":1}}}')`)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{}
	full, err := records.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	want := draftAlertsFromRecords(full, config)
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	for _, tc := range []struct {
		name, state string
		failOnNext  bool
	}{
		{"query_22P02", "22P02", false},
		{"query_22P05", "22P05", false},
		{"next_22P02", "22P02", true},
		{"next_22P05", "22P05", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts := &alertCounts{}
			db := sql.OpenDB(alertConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), counts})
			defer db.Close()
			s := &Service{options: Options{Records: &consultations.Records{DB: db}, Config: func(context.Context) (map[string]any, error) { return config, nil }}}
			for pass := 0; pass < 2; pass++ {
				*counts = alertCounts{failState: tc.state, failOnNext: tc.failOnNext}
				got, err := s.DraftAlerts(ctx)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("pass %d: got %#v, want %#v, err %v", pass, got, want, err)
				}
				assertFallbackQueries(t, counts.queries)
				wantRows := 4
				if tc.failOnNext {
					wantRows++ // One projected row arrived before the error.
				}
				if counts.rows != wantRows {
					t.Fatalf("pass %d: expected %d rows: %+v", pass, wantRows, counts)
				}
			}
			*counts = alertCounts{}
			if _, err := s.DraftAlerts(ctx); err != nil || len(counts.queries) != 2 || !isAlertProjection(counts.queries[1]) {
				t.Fatalf("recovery: %v %+v", err, counts)
			}
		})
	}
}

func TestDraftAlertsPostgresJSONFallback(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	writer, err := consultations.OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2),($3,$4)`,
		"bad", `{"id":"bad","status":"draft","parentSplit":"p","splitScopeHeld":true,"title":"\ud800"}`,
		"good", `{"id":"good","consultationBudget":{"stopped":{"calls":1}}}`)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{}
	full, err := writer.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	want := draftAlertsFromRecords(full, config)
	if len(want) != 2 {
		t.Fatalf("full read alerts: %#v", want)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	counts := &alertCounts{}
	db := sql.OpenDB(alertConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), counts})
	defer db.Close()
	s := &Service{options: Options{Records: &consultations.Records{DB: db}, Config: func(context.Context) (map[string]any, error) { return config, nil }}}
	for pass := 0; pass < 2; pass++ {
		*counts = alertCounts{}
		got, err := s.DraftAlerts(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d: got %#v, want %#v, err %v", pass, got, want, err)
		}
		assertFallbackQueries(t, counts.queries)
		if len(counts.projectionErrors) != 1 {
			t.Fatalf("projection errors: %+v", counts)
		}
		var pgErr *pgconn.PgError
		if !errors.As(counts.projectionErrors[0], &pgErr) || pgErr.Code != "22P02" {
			t.Fatalf("expected PostgreSQL 22P02: %v", counts.projectionErrors[0])
		}
		t.Logf("PostgreSQL 22P02 surfaced in %s after %d projected rows", counts.projectionErrorPhases[0], counts.projectionRows)
	}
	if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='bad'`, `{"id":"bad","status":"draft","parentSplit":"p","splitScopeHeld":true}`); err != nil {
		t.Fatal(err)
	}
	*counts = alertCounts{}
	if _, err := s.DraftAlerts(ctx); err != nil || len(counts.queries) != 2 || !isAlertProjection(counts.queries[1]) {
		t.Fatalf("recovery: %v %+v", err, counts)
	}
}
