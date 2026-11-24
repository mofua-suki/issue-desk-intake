package adminapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestDiagnosticNormalization(t *testing.T) {

	got := normalizeDiagnosticQuery("SELECT id FROM consultations WHERE body = 'secret' AND n > 42")
	for _, want := range []string{"id", "consultations", "body", "n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("識別子 %s がありません: %s", want, got)
		}
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "42") {
		t.Fatalf("値が残っています: %s", got)
	}
	if normalizeDiagnosticQuery("SELECT 非ASCII") != "" {
		t.Fatal("非ASCIIを返しました")
	}

}

func TestDiagnosticLexicalStatesAndTokenLimit(t *testing.T) {
	// PostgreSQL scan.l: xq, xe, xqs/quotecontinue, xdolq, xd, xus.
	cases := []struct{ name, input, want string }{
		{"xq doubled quote", `SELECT 'a''secret'`, "SELECT ?"},
		{"xq ambiguous backslash", `SELECT 'a\', secret`, ""},
		{"xe escaped quote", `SELECT E'a\'secret'`, "SELECT ?"},
		{"xe incomplete", `SELECT E'a\'`, ""},
		{"quotecontinue newline", "SELECT 'a'\n'secret'", "SELECT ?"},
		{"quotecontinue comment", "SELECT E'a' -- comment\n'\\'secret'", "SELECT ?"},
		{"quotecontinue no newline", "SELECT 'a' 'secret'", ""},
		{"xdolq tagged", `SELECT $tag$secret ' $other$ $tag$`, "SELECT ?"},
		{"xdolq incomplete", `SELECT $$secret`, ""},
		{"xd quoted identifier", `SELECT "secret""name"`, "SELECT ?"},
		{"xus string", `SELECT U&'secret'`, ""},
		{"xus identifier", `SELECT U&"secret"`, ""},
		{"nested block comment", `SELECT visible /* outer_secret /* inner_secret */ outer_secret */ FROM records`, "SELECT visible ? FROM records"},
		{"boundary exact", strings.Repeat("a", 256) + " x", strings.Repeat("a", 256)},
		{"boundary long token", "SELECT " + strings.Repeat("a", 250), "SELECT"},
		{"boundary first token", strings.Repeat("a", 257), ""},
		{"boundary operator", strings.Repeat("a", 254) + " <= x", strings.Repeat("a", 254)},
		{"boundary operator fits", strings.Repeat("a", 253) + " <= x", strings.Repeat("a", 253) + " <="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeDiagnosticQuery(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type diagnosticConnector struct {
	dsn     string
	options chan driver.TxOptions
	slow    bool
}

func (c diagnosticConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := stdlib.GetDefaultDriver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return diagnosticConn{Conn: conn, options: c.options, slow: c.slow}, nil
}
func (c diagnosticConnector) Driver() driver.Driver { return stdlib.GetDefaultDriver() }

type diagnosticConn struct {
	driver.Conn
	options chan driver.TxOptions
	slow    bool
}

func (c diagnosticConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.slow && query == activitySQL {
		query = "SELECT NULL::text, 0::float8, NULL::text, NULL::text FROM pg_sleep(4)"
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c diagnosticConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.options <- opts
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func TestDiagnosticsPostgres(t *testing.T) {
	f := newFixture(t)
	db, err := sql.Open("pgx", os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f.pg = db
	f.restart()
	for _, tc := range []struct {
		method, credential string
		want               int
	}{{"GET", "", 401}, {"GET", "worker", 403}, {"GET", "screen", 403}, {"POST", "delegate", 405}, {"GET", "delegate", 200}, {"GET", "cli", 200}} {
		code, value := f.call(tc.method, "/api/diagnostics/db", tc.credential, nil)
		status(t, code, tc.want, value)
	}
	t.Setenv("ISSUE_DESK_COLOR", "blue")
	code, value := f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	identity := obj(value)
	if identity["color"] != "blue" || identity["instanceId"] == "" || identity["startedAt"] == "" || identity["pid"].(json.Number) == "0" {
		t.Fatal("受付の識別が不足しています")
	}
	t.Setenv("ISSUE_DESK_COLOR", "")
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	if obj(value)["color"] != "unknown" || obj(value)["instanceId"] != identity["instanceId"] {
		t.Fatal("受付の識別が不正です")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = db.ExecContext(ctx, "SELECT pg_sleep(20), 'diagnostic_secret_74', 8712345 /* diagnostic_comment_74 */")
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		err = db.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid <> pg_backend_pid() AND query LIKE '%diagnostic_secret_74%' AND state='active'").Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("実行中クエリが見つかりません")
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	raw, _ := json.Marshal(value)
	text := string(raw)
	if strings.Contains(text, "diagnostic_secret_74") || strings.Contains(text, "diagnostic_comment_74") {
		t.Fatal("応答にリテラルが含まれます")
	}
	v := obj(value)
	// 数値のリテラルはクエリ文だけで照合する。応答全体では経過秒数・時刻・pidの数字に一致しうる。
	for _, raw := range v["queries"].([]any) {
		if query, _ := obj(raw)["query"].(string); strings.Contains(query, "8712345") {
			t.Fatal("クエリに数値のリテラルが含まれます")
		}
	}
	if len(v["queries"].([]any)) == 0 || len(v["connections"].([]any)) == 0 || v["maxConnections"].(json.Number) == "0" {
		t.Fatal("診断結果が不足しています")
	}
	if !strings.Contains(text, "pg_sleep") {
		t.Fatal("実行中の関数名がありません")
	}
	// The observed session may use a different string mode than the diagnostic session.
	observedOff, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer observedOff.Close()
	if _, err := observedOff.ExecContext(ctx, "SET standard_conforming_strings = off"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	offDone := make(chan struct{})
	go func() {
		defer close(offDone)
		_, _ = observedOff.ExecContext(ctx, `SELECT pg_sleep(20), 'escaped\' observed_secret_91'`)
	}()
	defer func() { cancel(); <-offDone }()
	for {
		var n int
		err = db.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE query LIKE '%observed_secret_91%' AND state='active' AND pid <> pg_backend_pid()").Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("off接続のクエリが見つかりません")
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	raw, _ = json.Marshal(value)
	if strings.Contains(string(raw), "observed_secret_91") {
		t.Fatal("別接続の秘密が返されました")
	}
	omitted := false
	for _, entry := range obj(value)["queries"].([]any) {
		if _, present := obj(entry)["query"]; !present {
			omitted = true
		}
	}
	if !omitted {
		t.Fatal("設定が異なる接続の本文が省略されませんでした")
	}
	// The setting belongs to the diagnostic connection, not the observed one.
	off, err := sql.Open("pgx", os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	off.SetMaxOpenConns(1)
	if _, err := off.Exec("SET standard_conforming_strings = off"); err != nil {
		t.Fatal(err)
	}
	f.pg = off
	f.restart()
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	if len(obj(value)["queries"].([]any)) == 0 {
		t.Fatal("標本がありません")
	}
	for _, entry := range obj(value)["queries"].([]any) {
		if _, present := obj(entry)["query"]; present {
			t.Fatal("設定offで本文が返されました")
		}
	}
	// pg_stat_activity is database-scoped, not schema-scoped. Other packages
	// use the same test database concurrently, so give this assertion its own.
	testDBName := "issue_desk_test_diagnostics_" + strings.ReplaceAll(randomID(), "-", "")
	quotedDBName := pgx.Identifier{testDBName}.Sanitize()
	if _, err := db.Exec("CREATE DATABASE " + quotedDBName + " TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec("DROP DATABASE " + quotedDBName); err != nil {
			t.Error(err)
		}
	})
	idleURL, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	idleURL.Path = "/" + testDBName
	idleDSN := idleURL.String()
	// A separate role cannot see the superuser's active query or state.
	if _, err := db.Exec("CREATE ROLE diagnostic_unprivileged NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
		if err != nil {
			t.Error(err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec("DROP ROLE diagnostic_unprivileged"); err != nil {
			t.Error(err)
		}
	})
	limited, err := sql.Open("pgx", idleDSN)
	if err != nil {
		t.Fatal(err)
	}
	limited.SetMaxOpenConns(1)
	defer limited.Close()
	if _, err := limited.Exec("SET ROLE diagnostic_unprivileged"); err != nil {
		t.Fatal(err)
	}
	f.pg = limited
	f.restart()
	countUnknown := func(value any) int {
		entries := obj(value)["queries"].([]any)
		if len(entries) >= 100 {
			t.Fatal("標本が上限に達しており増分を検証できません")
		}
		count := 0
		for _, entry := range entries {
			item := obj(entry)
			if item["state"] == "unknown" {
				if _, present := item["query"]; present {
					t.Fatal("権限不足の本文が返されました")
				}
				count++
			}
		}
		return count
	}
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	before := countUnknown(value)
	// A fresh pool guarantees this backend was not already in the baseline.
	idleDB, err := sql.Open("pgx", idleDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer idleDB.Close()
	idle, err := idleDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	var idlePID int
	if err := idle.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&idlePID); err != nil {
		t.Fatal(err)
	}
	var actualState string
	var visibleState sql.NullString
	if err := db.QueryRow("SELECT state FROM pg_stat_activity WHERE pid=$1", idlePID).Scan(&actualState); err != nil || actualState != "idle" {
		t.Fatalf("idle接続を確認できません: state=%q err=%v", actualState, err)
	}
	if err := limited.QueryRow("SELECT state FROM pg_stat_activity WHERE pid=$1", idlePID).Scan(&visibleState); err != nil || visibleState.Valid {
		t.Fatalf("権限不足のidle接続が非表示ではありません: state=%v err=%v", visibleState, err)
	}
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	if after := countUnknown(value); after != before+1 {
		t.Fatalf("権限不足のidle接続による標本の増分が不正です: before=%d after=%d", before, after)
	}

	// Exercise the endpoint through a connector that records its actual transaction options.
	options := make(chan driver.TxOptions, 1)
	observed := sql.OpenDB(diagnosticConnector{dsn: os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"), options: options})
	defer observed.Close()
	f.pg = observed
	f.restart()
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 200, value)
	select {
	case opts := <-options:
		if !opts.ReadOnly {
			t.Fatal("診断トランザクションが読み取り専用ではありません")
		}
	default:
		t.Fatal("診断トランザクションが開始されませんでした")
	}
	slowOptions := make(chan driver.TxOptions, 1)
	slow := sql.OpenDB(diagnosticConnector{dsn: os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"), options: slowOptions, slow: true})
	defer slow.Close()
	f.pg = slow
	f.restart()
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 503, value)
	f.pg = db
	f.restart()
	db.Close()
	code, value = f.call("GET", "/api/diagnostics/db", "delegate", nil)
	status(t, code, 503, value)
}
