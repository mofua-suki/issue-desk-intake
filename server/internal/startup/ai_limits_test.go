package startup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

func TestRecordLimitsPostgres(t *testing.T) {
	db := appPostgres(t)
	app := &App{DB: db, ctx: context.Background()}
	app.recordLimits([]ai.LimitObservation{{"provider": "claude", "limit": "five_hour", "status": "allowed", "utilization": 0.25, "percent": 25.0, "resetsAt": "2027-01-01T00:00:00.000Z", "windowMinutes": nil}, {"provider": "codex", "limit": "seven_day", "status": nil, "utilization": nil, "percent": 45.0, "resetsAt": nil, "windowMinutes": 10080}})
	rows, err := db.Query(`SELECT json FROM usage_limit_rows WHERE source='intake' AND host='nas' ORDER BY provider`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0]["provider"] != "claude" || values[0]["percent"] != float64(25) || values[1]["provider"] != "codex" || values[1]["windowMinutes"] != float64(10080) {
		t.Fatal(values)
	}
}

type limitTestConnector struct {
	rows []map[string]any
	fail bool
}

func (c *limitTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &limitTestConn{c}, nil
}
func (c *limitTestConnector) Driver() driver.Driver { return limitTestDriver{} }

type limitTestDriver struct{}

func (limitTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector required")
}

type limitTestConn struct{ c *limitTestConnector }

func (*limitTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*limitTestConn) Close() error              { return nil }
func (*limitTestConn) Begin() (driver.Tx, error) { return limitTestTx{}, nil }
func (c *limitTestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.c.fail {
		return nil, errors.New("save failed")
	}
	if !strings.Contains(query, "INSERT INTO usage_limit_rows") || len(args) != 8 {
		return nil, errors.New("unexpected insert")
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(args[6].Value.(string)), &row); err != nil {
		return nil, err
	}
	c.c.rows = append(c.c.rows, row)
	return driver.RowsAffected(1), nil
}

type limitTestTx struct{}

func (limitTestTx) Commit() error   { return nil }
func (limitTestTx) Rollback() error { return nil }

func TestRealLimitSamplesThroughRunnerAndStore(t *testing.T) {
	const claude = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794024000,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.29,"resetsAt":1794024000,"status":"allowed"}}}}`
	const codex = `{"timestamp":"2026-11-03T08:02:27.965Z","ordinal":144,"type":"event_msg","payload":{"type":"token_count","info":{},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":22.0,"window_minutes":10080,"resets_at":1794126998},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"individual_limit":null,"spend_control_reached":null,"plan_type":"pro","rate_limit_reached_type":null}}}`
	connector := &limitTestConnector{}
	db := sql.OpenDB(connector)
	defer db.Close()
	app := &App{DB: db, ctx: context.Background()}
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := "12345678-1234-1234-1234-123456789abc"
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessionDir := filepath.Join(home, "sessions", "2026", "11", "03")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Captured structure and values, with the timestamp shifted into this test call.
	rollout := strings.Replace(codex, "2026-11-03T08:02:27.965Z", time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), 1)
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout-2026-11-03-"+id+".jsonl"), []byte(rollout), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &ai.Runner{ObserveLimits: app.recordLimits, Execute: func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
		if p.Arguments[0] == "auth" {
			return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
		}
		if p.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		if p.Executable == "claude" {
			return ai.ProcessResult{Stdout: []byte(claude)}, errors.New("simulated failed call")
		}
		return ai.ProcessResult{Stdout: []byte(`{"type":"thread.started","thread_id":"` + id + `"}`)}, errors.New("simulated failed call")
	}}
	req := ai.Request{Provider: "claude", Model: "fixture", Workspace: dir, SchemaFile: schema}
	if _, err := runner.Call(context.Background(), req); err == nil {
		t.Fatal("expected failed Claude call")
	}
	req.Provider = "codex"
	req.PersistSession = true
	if _, err := runner.Call(context.Background(), req); err == nil {
		t.Fatal("expected failed Codex call")
	}
	if len(connector.rows) != 2 {
		t.Fatalf("saved rows: %#v", connector.rows)
	}
	for _, row := range connector.rows {
		if !usage.ValidLimit(row) || row["host"] != "nas" || row["source"] != "intake" || len(row) != 10 {
			t.Fatalf("invalid row: %#v", row)
		}
	}
	c, x := connector.rows[0], connector.rows[1]
	if c["provider"] != "claude" || c["limit"] != "seven_day" || c["percent"] != float64(29) || c["utilization"] != 0.29 || c["status"] != "allowed" {
		t.Fatal(c)
	}
	if x["provider"] != "codex" || x["limit"] != "seven_day" || x["percent"] != float64(22) || x["windowMinutes"] != float64(10080) || x["utilization"] != nil {
		t.Fatal(x)
	}
	req.PersistSession = false
	runner.Call(context.Background(), req)
	if len(connector.rows) != 2 {
		t.Fatal("ephemeral saved a limit", connector.rows)
	}
}

func TestLimitStorageFailureDoesNotStopCall(t *testing.T) {
	connector := &limitTestConnector{fail: true}
	db := sql.OpenDB(connector)
	defer db.Close()
	logged := false
	app := &App{DB: db, ctx: context.Background(), Log: func(string) { logged = true }}
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &ai.Runner{ObserveLimits: app.recordLimits, Execute: func(_ context.Context, p ai.ProcessRequest) (ai.ProcessResult, error) {
		if p.Arguments[0] == "auth" {
			return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
		}
		return ai.ProcessResult{Stdout: []byte(`{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"five_hour","utilization":0.2,"status":"allowed"}}
{"type":"result","structured_output":{"ok":true}}`)}, nil
	}}
	answer, err := runner.Call(context.Background(), ai.Request{Provider: "claude", Model: "fixture", Workspace: dir, SchemaFile: schema})
	if err != nil || answer.Value["ok"] != true || !logged {
		t.Fatalf("answer=%v err=%v logged=%v", answer.Value, err, logged)
	}
}
