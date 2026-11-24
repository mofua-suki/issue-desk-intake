package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
)

// Each expectation names SQL independently of the production SQL generator.
// Fake is used as the autocommit runner as well as the transaction provider.
func TestApplicationFakeOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.UnixMilli(1793490000123)
	stamp := now.UTC().Format(time.RFC3339Nano)
	token := PCTokenRecord{ID: "id", Name: "pc", Hash: "hash", IssuedAt: now}
	type operation struct {
		name, postgres string
		args           []any
		values         [][]any
		want           any
		run            func(*SQLite) (any, error)
	}
	operations := []operation{
		{"setting", "SELECT json FROM settings WHERE id=$1", []any{"key"}, [][]any{{`{"keep":1}`}}, json.RawMessage(`{"keep":1}`), func(s *SQLite) (any, error) {
			v, ok, e := s.Setting(ctx, "key")
			if e == nil && !ok {
				return nil, errors.New("missing setting")
			}
			return v, e
		}},
		{"put-setting", "INSERT INTO settings(id,json) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET json=excluded.json", []any{"key", `{"keep":1}`}, nil, nil, func(s *SQLite) (any, error) { return nil, s.PutSetting(ctx, "key", map[string]int{"keep": 1}) }},
		{"add-token", "INSERT INTO api_tokens(id,name,hash,issued_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", []any{"id", "pc", "hash", stamp}, nil, true, func(s *SQLite) (any, error) { return s.AddPCToken(ctx, token) }},
		{"token", "SELECT t.id,t.name,CASE WHEN g.token_id IS NULL THEN 0 ELSE 1 END\nFROM api_tokens t LEFT JOIN api_token_grants g ON t.id=g.token_id WHERE t.hash=$1", []any{"hash"}, [][]any{{"id", "pc", 1}}, auth.Token{ID: "id", Name: "pc", Delegated: true}, func(s *SQLite) (any, error) {
			v, ok, e := s.PCToken(ctx, "hash")
			if e == nil && !ok {
				return nil, errors.New("missing token")
			}
			return v, e
		}},
		{"token-by-id", "SELECT t.id,t.name,CASE WHEN g.token_id IS NULL THEN 0 ELSE 1 END FROM api_tokens t LEFT JOIN api_token_grants g ON t.id=g.token_id WHERE t.id=$1", []any{"id"}, [][]any{{"id", "pc", 1}}, auth.Token{ID: "id", Name: "pc", Delegated: true}, func(s *SQLite) (any, error) {
			v, ok, e := s.TokenByID(ctx, "id")
			if e == nil && !ok {
				return nil, errors.New("missing token")
			}
			return v, e
		}},
		{"touch-token", "UPDATE api_tokens SET last_used_at=$1 WHERE id=$2", []any{stamp, "id"}, nil, nil, func(s *SQLite) (any, error) { return nil, s.TouchPCToken(ctx, "id", now) }},
		{"revoke-token", "DELETE FROM api_tokens WHERE name=$1", []any{"pc"}, nil, true, func(s *SQLite) (any, error) { return s.RevokePCToken(ctx, "pc") }},
		{"delegate", "INSERT INTO api_token_grants (token_id, granted_at) SELECT id,$1 FROM api_tokens WHERE id=$2 ON CONFLICT DO NOTHING", []any{stamp, "id"}, nil, true, func(s *SQLite) (any, error) { return s.SetDelegated(ctx, "id", true, now) }},
		{"undelegate", "DELETE FROM api_token_grants WHERE token_id=$1", []any{"id"}, nil, true, func(s *SQLite) (any, error) { return s.SetDelegated(ctx, "id", false, now) }},
		{"add-cli", "INSERT INTO cli_keys(hash,expires) VALUES($1,$2) ON CONFLICT(hash) DO UPDATE SET expires=excluded.expires", []any{"hash", now.UnixMilli()}, nil, nil, func(s *SQLite) (any, error) { return nil, s.AddCLIKey(ctx, "hash", now) }},
		{"consume-cli", "DELETE FROM cli_keys WHERE hash=$1 AND expires>=$2", []any{"hash", now.UnixMilli()}, nil, true, func(s *SQLite) (any, error) { return s.ConsumeCLIKey(ctx, "hash", now) }},
		{"consume-desktop", "UPDATE settings SET json='null' WHERE id='desktopToken' AND json::jsonb->>'hash'=$1 AND (json::jsonb->>'expires')::numeric>=$2", []any{"hash", now.UnixMilli()}, nil, true, func(s *SQLite) (any, error) { return s.ConsumeDesktopKey(ctx, "hash", now) }},
		{"session", "SELECT json FROM settings WHERE id=$1", []any{"sessions"}, [][]any{{`[["hash",{"csrf":"csrf","expires":1793490000123,"future":7}]]`}}, auth.Session{CSRF: "csrf", ExpiresAt: now}, func(s *SQLite) (any, error) {
			v, ok, e := s.Session(ctx, "hash")
			if e == nil && !ok {
				return nil, errors.New("missing session")
			}
			return v, e
		}},
	}
	for _, dialect := range []appsql.Dialect{appsql.Postgres} {
		for _, op := range operations {
			for _, mode := range []string{"success", "error", "zero", "count-error"} {
				if op.values != nil && (mode == "zero" || mode == "count-error") {
					continue
				}
				t.Run(fmt.Sprintf("%d/%s/%s", dialect, op.name, mode), func(t *testing.T) {
					q := op.postgres
					rows := &appsql.FakeRows{Values: op.values}
					step := appsql.Step{Operation: "exec", SQL: q, Args: op.args, Context: ctx, Result: appsql.FakeResult{Affected: 1}}
					if op.values != nil {
						step.Operation = "query"
						step.Rows = rows
					}
					sentinel := errors.New("operation failed")
					switch mode {
					case "error":
						step.Err = sentinel
					case "zero":
						step.Result = appsql.FakeResult{}
					case "count-error":
						step.Result = appsql.FakeResult{AffectedErr: sentinel}
					}
					f := &appsql.Fake{Steps: []appsql.Step{step}}
					s := &SQLite{db: &applicationDB{dialect: dialect, runner: f, transactions: f}}
					got, err := op.run(s)
					boolResult, _ := op.want.(bool)
					counts := boolResult || op.name == "touch-token"
					wantErr := mode == "error" || (mode == "count-error" && counts)
					if wantErr {
						if !errors.Is(err, sentinel) {
							t.Fatal("operation error not preserved")
						}
					} else if mode == "zero" && op.name == "touch-token" {
						if err == nil {
							t.Fatal("missing token touch accepted")
						}
					} else {
						want := op.want
						if mode == "zero" && boolResult {
							want = false
						}
						if err != nil || !reflect.DeepEqual(got, want) {
							t.Fatalf("unexpected operation result: %v", err)
						}
					}
					if mode != "error" && op.values != nil && !rows.Closed {
						t.Fatal("rows not closed")
					}

					if e := f.Verify(); e != nil {
						t.Fatal(e)
					}
				})
			}
		}
	}
}

func TestApplicationFakeSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.UnixMilli(1793490000123)
	before := `[["hash",{"csrf":"old","expires":1793490000123,"future":7}],["other",{"csrf":"keep","expires":1793490000123}]]`
	cases := []struct {
		name, after string
		run         func(*SQLite) error
	}{
		{"put", `[["hash",{"csrf":"new","expires":1793490000123,"future":7}],["other",{"csrf":"keep","expires":1793490000123}]]`, func(s *SQLite) error { return s.PutSession(ctx, "hash", auth.Session{CSRF: "new", ExpiresAt: now}) }},
		{"revoke", `[["other",{"csrf":"keep","expires":1793490000123}]]`, func(s *SQLite) error { return s.RevokeSession(ctx, "hash") }},
		{"create", `[["other",{"csrf":"keep","expires":1793490000123}],["hash",{"csrf":"new","expires":1793490000123}]]`, func(s *SQLite) error {
			return s.CreateSession(ctx, "hash", auth.Session{CSRF: "new", ExpiresAt: now}, now)
		}},
	}
	for _, d := range []appsql.Dialect{appsql.Postgres} {
		for _, tc := range cases {
			for _, failure := range []string{"", "begin", "query", "read", "scan", "close", "decode", "edit", "exec", "commit"} {
				t.Run(fmt.Sprintf("%d/%s/%s", d, tc.name, failure), func(t *testing.T) {
					sentinel := errors.New("transaction failed")
					q := "INSERT INTO settings(id,json) VALUES('sessions',$1) ON CONFLICT(id) DO UPDATE SET json=excluded.json"
					rows := &closingApplicationRows{FakeRows: appsql.FakeRows{Values: [][]any{{before}}}}
					switch failure {
					case "read":
						rows.Error = sentinel
					case "scan":
						rows.Values = [][]any{{42}}
					case "close":
						rows.closeErr = sentinel
					case "decode":
						rows.Values = [][]any{{`"invalid"`}}
					case "edit":
						rows.Values = [][]any{{`[["hash",null]]`}}
					}
					steps := []appsql.Step{{Operation: "begin", Context: ctx}}
					if failure == "begin" {
						steps[0].Err = sentinel
					} else {
						query := appsql.Step{Operation: "query", Context: ctx, SQL: "SELECT json FROM settings WHERE id='sessions'", Rows: rows}
						if failure == "query" {
							query.Err = sentinel
						}
						steps = append(steps, query)
						// revoke does not decode the value being removed, and null decodes as a
						// zero-valued CreateSession entry which is pruned by its existing rule.
						editFails := failure == "edit" && tc.name == "put"
						callbackFails := failure == "query" || failure == "read" || failure == "scan" || failure == "close" || failure == "decode" || editFails
						if !callbackFails {
							after := tc.after
							if failure == "edit" {
								if tc.name == "revoke" {
									after = `[]`
								} else {
									after = `[["hash",{"csrf":"new","expires":1793490000123}]]`
								}
							}
							step := appsql.Step{Operation: "exec", Context: ctx, SQL: q, Args: []any{after}, Result: appsql.FakeResult{Affected: 1}}
							if failure == "exec" {
								step.Err = sentinel
							}
							steps = append(steps, step)
							if failure != "exec" {
								step = appsql.Step{Operation: "commit"}
								if failure == "commit" {
									step.Err = sentinel
								}
								steps = append(steps, step)
							}
						}
						steps = append(steps, appsql.Step{Operation: "rollback"})
					}
					f := &appsql.Fake{Steps: steps}
					s := &SQLite{db: &applicationDB{dialect: d, runner: f, transactions: f}}
					err := tc.run(s)
					wantError := failure != "" && !(failure == "edit" && tc.name != "put")
					if (err != nil) != wantError {
						t.Fatalf("unexpected transaction result: %v", err)
					}
					if failure == "begin" || failure == "query" || failure == "read" || failure == "close" || failure == "exec" || failure == "commit" {
						if !errors.Is(err, sentinel) {
							t.Fatal("transaction error not preserved")
						}
					}
					if failure != "begin" && failure != "query" && !rows.Closed {
						t.Fatal("rows not closed before transaction completion")
					}
					if e := f.Verify(); e != nil {
						t.Fatal(e)
					}
				})
			}
		}
	}
}

type closingApplicationRows struct {
	appsql.FakeRows
	closeErr error
}

func (r *closingApplicationRows) Close() error { r.FakeRows.Close(); return r.closeErr }

func TestApplicationFakeMissingAndResultErrors(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1793490000123)
	for _, d := range []appsql.Dialect{appsql.Postgres} {
		t.Run(fmt.Sprint(d), func(t *testing.T) {
			query := "SELECT json FROM settings WHERE id=$1"
			for _, rowErr := range []error{nil, errors.New("read failed")} {
				rows := &appsql.FakeRows{Error: rowErr}
				f := &appsql.Fake{Steps: []appsql.Step{{Operation: "query", SQL: query, Args: []any{"missing"}, Rows: rows}}}
				s := &SQLite{db: &applicationDB{dialect: d, runner: f}}
				_, found, err := s.Setting(ctx, "missing")
				if found || !errors.Is(err, rowErr) || !rows.Closed {
					t.Fatal("missing or failed row mishandled")
				}
				if e := f.Verify(); e != nil {
					t.Fatal(e)
				}
			}
			q := "DELETE FROM cli_keys WHERE hash=$1 AND expires>=$2"
			for _, affectedErr := range []error{nil, errors.New("count failed")} {
				f := &appsql.Fake{Steps: []appsql.Step{{Operation: "exec", SQL: q, Args: []any{"missing", now.UnixMilli()}, Result: appsql.FakeResult{AffectedErr: affectedErr}}}}
				s := &SQLite{db: &applicationDB{dialect: d, runner: f}}
				ok, err := s.ConsumeCLIKey(ctx, "missing", now)
				if ok || !errors.Is(err, affectedErr) {
					t.Fatal("affected-row result mishandled")
				}
				if e := f.Verify(); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
	// Both malformed Bind input and InsertIgnore generation errors stop before I/O.
	f := &appsql.Fake{}
	db := &applicationDB{dialect: appsql.Dialect(-1), runner: f}
	if _, err := db.ExecContext(ctx, "UPDATE settings SET json=?", "secret"); err == nil {
		t.Fatal("invalid dialect accepted")
	}
	if err := db.QueryRowContext(ctx, "SELECT ?", "secret").Scan(new(string)); err == nil {
		t.Fatal("invalid query dialect accepted")
	}
	if _, err := (&SQLite{db: db}).SetDelegated(ctx, "secret", true, now); err == nil {
		t.Fatal("invalid insert dialect accepted")
	}
	db.dialect = appsql.Postgres
	if _, err := db.ExecContext(ctx, "SELECT '", "secret"); err == nil {
		t.Fatal("invalid SQL accepted")
	}
	if e := f.Verify(); e != nil {
		t.Fatal(e)
	}
}

// This contract runs against the PostgreSQL test schema derived from the shipped
// application schema. Fake expectations alone cannot prove SQL validity.
func checkApplicationContract(t *testing.T, s *SQLite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.UnixMilli(1793490000123)
	checkBool := func(label string, got, want bool, err error) {
		t.Helper()
		if err != nil || got != want {
			t.Fatalf("%s: got %t want %t, error %v", label, got, want, err)
		}
	}
	if _, ok, err := s.Setting(ctx, "missing"); err != nil || ok {
		t.Fatal("missing setting")
	}
	for _, v := range []int{1, 2} {
		if err := s.PutSetting(ctx, "unrelated", map[string]int{"keep": v}); err != nil {
			t.Fatal(err)
		}
	}
	raw, ok, err := s.Setting(ctx, "unrelated")
	if err != nil || !ok || string(raw) != `{"keep":2}` {
		t.Fatal("setting upsert")
	}
	token := PCTokenRecord{ID: "id", Name: "pc", Hash: "hash", IssuedAt: now}
	added, err := s.AddPCToken(ctx, token)
	checkBool("add", added, true, err)
	// Each independently unique field must suppress only a uniqueness conflict.
	for _, duplicate := range []PCTokenRecord{token, {ID: "other", Name: "pc", Hash: "other", IssuedAt: now}, {ID: "other", Name: "other", Hash: "hash", IssuedAt: now}, {ID: "id", Name: "other", Hash: "other", IssuedAt: now}} {
		added, err = s.AddPCToken(ctx, duplicate)
		checkBool("duplicate", added, false, err)
	}
	changed, err := s.SetDelegated(ctx, "missing", true, now)
	checkBool("missing delegate", changed, false, err)
	changed, err = s.SetDelegated(ctx, "id", true, now)
	checkBool("delegate", changed, true, err)
	changed, err = s.SetDelegated(ctx, "id", true, now)
	checkBool("duplicate grant", changed, false, err)
	got, ok, err := s.PCToken(ctx, "hash")
	if err != nil || !ok || got.ID != "id" || !got.Delegated {
		t.Fatal("delegated token")
	}
	byID, foundByID, lookupErr := s.TokenByID(ctx, "id")
	if lookupErr != nil || !foundByID || byID != got {
		t.Fatal("token lookup by id")
	}
	names, lookupErr := s.TokenNames(ctx)
	if lookupErr != nil || !reflect.DeepEqual(names, []string{"pc"}) {
		t.Fatal("token names")
	}
	if err := s.TouchPCToken(ctx, "id", now); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchPCToken(ctx, "missing", now); err == nil {
		t.Fatal("touch missing token")
	}
	changed, err = s.SetDelegated(ctx, "id", false, now)
	checkBool("undelegate", changed, true, err)
	changed, err = s.SetDelegated(ctx, "id", false, now)
	checkBool("missing grant", changed, false, err)
	got, ok, err = s.PCToken(ctx, "hash")
	if err != nil || !ok || got.Delegated {
		t.Fatal("grant not removed")
	}
	removed, err := s.RevokePCToken(ctx, "pc")
	checkBool("revoke", removed, true, err)
	removed, err = s.RevokePCToken(ctx, "pc")
	checkBool("missing revoke", removed, false, err)
	if _, ok, err = s.PCToken(ctx, "hash"); err != nil || ok {
		t.Fatal("revoked token still found")
	}
	for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		if err := s.AddCLIKey(ctx, "cli", now.Add(delta)); err != nil {
			t.Fatal(err)
		}
		used, err := s.ConsumeCLIKey(ctx, "cli", now)
		checkBool("cli boundary", used, delta >= 0, err)
		used, err = s.ConsumeCLIKey(ctx, "cli", now)
		checkBool("cli second consumption", used, false, err)
	}
	if err := s.AddCLIKey(ctx, "upsert", now.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCLIKey(ctx, "upsert", now); err != nil {
		t.Fatal(err)
	}
	used, err := s.ConsumeCLIKey(ctx, "upsert", now)
	checkBool("cli upsert", used, true, err)
	used, err = s.ConsumeDesktopKey(ctx, "hash", now)
	checkBool("missing desktop", used, false, err)
	for _, value := range []any{nil, map[string]any{}, map[string]any{"hash": "hash"}, map[string]any{"hash": "wrong", "expires": now.UnixMilli()}} {
		if err := s.PutSetting(ctx, "desktopToken", value); err != nil {
			t.Fatal(err)
		}
		used, err = s.ConsumeDesktopKey(ctx, "hash", now)
		checkBool("incomplete desktop", used, false, err)
	}
	for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		if err := s.PutSetting(ctx, "desktopToken", map[string]any{"hash": "hash", "expires": now.Add(delta).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		used, err = s.ConsumeDesktopKey(ctx, "hash", now)
		checkBool("desktop boundary", used, delta >= 0, err)
		if delta >= 0 {
			raw, ok, err = s.Setting(ctx, "desktopToken")
			if err != nil || !ok || string(raw) != "null" {
				t.Fatal("desktop not stored as JSON null")
			}
		}
		used, err = s.ConsumeDesktopKey(ctx, "hash", now)
		checkBool("desktop second consumption", used, false, err)
	}
	// The PostgreSQL numeric cast rejects a non-numeric expires value, and invalid
	// JSON is an error too; neither may overwrite the stored value.
	for _, bad := range []struct{ name, raw string }{
		{"non-numeric expires", `{"hash":"hash","expires":"x"}`},
		{"invalid JSON", `{"hash":"hash",`},
	} {
		if _, err := s.db.ExecContext(ctx, "UPDATE settings SET json=? WHERE id='desktopToken'", bad.raw); err != nil {
			t.Fatal(err)
		}
		used, err := s.ConsumeDesktopKey(ctx, "hash", now)
		if used || err == nil {
			t.Fatalf("%s: 不正データに対する現行動作と異なります", bad.name)
		}
		wantSaved := bad.raw
		var saved string
		if err := s.db.QueryRowContext(ctx, "SELECT json FROM settings WHERE id='desktopToken'").Scan(&saved); err != nil || saved != wantSaved {
			t.Fatal("デスクトップキーのエラー時の非上書きまたは消費後の保存形式が変わりました")
		}
	}
	session := auth.Session{CSRF: "csrf", ExpiresAt: now}
	if err := s.PutSetting(ctx, "sessions", json.RawMessage(`[["cookie",{"csrf":"old","expires":1,"future":{"keep":true}}],["other",{"csrf":"keep","expires":1793490000123}]]`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSession(ctx, "cookie", session); err != nil {
		t.Fatal(err)
	}
	gotSession, ok, err := s.Session(ctx, "cookie")
	if err != nil || !ok || gotSession != session {
		t.Fatal("session update")
	}
	raw, _, err = s.Setting(ctx, "sessions")
	if err != nil || !strings.Contains(string(raw), `"future":{"keep":true}`) {
		t.Fatal("unknown session field lost")
	}
	if err := s.RevokeSession(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "cookie"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = s.Session(ctx, "cookie"); err != nil || ok {
		t.Fatal("revoked session")
	}
	if _, ok, err = s.Session(ctx, "other"); err != nil || !ok {
		t.Fatal("other session lost")
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`"bad"`), json.RawMessage(`[["cookie"]]`), json.RawMessage(`[["cookie",null]]`)} {
		if err := s.PutSetting(ctx, "sessions", bad); err != nil {
			t.Fatal(err)
		}
		if err := s.PutSession(ctx, "cookie", session); err == nil {
			t.Fatal("invalid session overwritten")
		}
		raw, _, err = s.Setting(ctx, "sessions")
		if err != nil || string(raw) != string(bad) {
			t.Fatal("invalid data changed on failure")
		}
	}
	// Invalid raw JSON may exist in an old settings row. Neither a read nor an
	// edit is allowed to repair it silently.
	if _, err := s.db.ExecContext(ctx, "UPDATE settings SET json=? WHERE id='sessions'", "{"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Session(ctx, "cookie"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if err := s.CreateSession(ctx, "cookie", session, now); err == nil {
		t.Fatal("invalid JSON overwritten")
	}
	var saved string
	if err := s.db.QueryRowContext(ctx, "SELECT json FROM settings WHERE id='sessions'").Scan(&saved); err != nil || saved != "{" {
		t.Fatal("invalid JSON changed")
	}
	// Creation keeps the last 100 surviving entries followed by the new one.
	var pairs []sessionPair
	pairs = append(pairs, sessionPair{Hash: "expired", Value: json.RawMessage(`{"expires":1793490000122}`)})
	for i := 0; i < 103; i++ {
		pairs = append(pairs, sessionPair{Hash: fmt.Sprintf("s%03d", i), Value: json.RawMessage(`{"csrf":"keep","expires":1793490000123,"future":7}`)})
	}
	encoded, err := encodeSessions(pairs)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "sessions", json.RawMessage(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "s050", session, now); err != nil {
		t.Fatal(err)
	}
	raw, _, err = s.Setting(ctx, "sessions")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := decodeSessions(raw)
	if err != nil || len(retained) != 101 || retained[0].Hash != "s002" || retained[99].Hash != "s102" || retained[100].Hash != "s050" {
		t.Fatal("session creation order or count changed")
	}
	if !strings.Contains(string(retained[0].Value), `"future":7`) {
		t.Fatal("retained session fields lost")
	}
	raw, ok, err = s.Setting(ctx, "unrelated")
	if err != nil || !ok || string(raw) != `{"keep":2}` {
		t.Fatal("unrelated setting changed")
	}
}

func checkApplicationConstraintContract(t *testing.T, s *SQLite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.UnixMilli(1793490000123)
	if _, err := s.AddPCToken(ctx, PCTokenRecord{ID: "constraint", Name: "constraint", Hash: "constraint", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	// This exact INSERT shape must reject NOT NULL violations.
	// Replacing ON CONFLICT with OR IGNORE would silently accept this operation.
	if _, err := s.db.ExecContext(ctx, "INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", "not-null", nil, "not-null", now.UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("AddPCTokenのSQLがNOT NULL違反を無視しました")
	}
	q, err := s.db.dialect.InsertIgnore("api_token_grants", []string{"token_id", "granted_at"}, "SELECT id,? FROM api_tokens WHERE id=?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, q, nil, "constraint"); err == nil {
		t.Fatal("PostgreSQLのInsertIgnoreはNOT NULL違反をエラーにするはずです")
	}
	// The real method always supplies non-null strings and selects an existing
	// id, so it succeeds over all values that its public API can produce.
	changed, err := s.SetDelegated(ctx, "constraint", true, now)
	if err != nil || !changed {
		t.Fatal("通常の委任登録に失敗しました")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM api_token_grants WHERE token_id=?", "constraint").Scan(&count); err != nil || count != 1 {
		t.Fatal("委任の保存結果が一致しません")
	}
}

func TestApplicationFakeEmptySessionsAndPanicRollback(t *testing.T) {
	ctx := context.Background()
	for _, d := range []appsql.Dialect{appsql.Postgres} {
		for _, mode := range []string{"empty", "empty-close-error", "panic"} {
			t.Run(fmt.Sprintf("%d/%s", d, mode), func(t *testing.T) {
				sentinel := errors.New("close failed")
				rows := &closingApplicationRows{}
				if mode == "empty-close-error" {
					rows.closeErr = sentinel
				}
				steps := []appsql.Step{{Operation: "begin", Context: ctx}, {Operation: "query", Context: ctx, SQL: "SELECT json FROM settings WHERE id='sessions'", Rows: rows}}
				if mode == "empty" {
					q := "INSERT INTO settings(id,json) VALUES('sessions',$1) ON CONFLICT(id) DO UPDATE SET json=excluded.json"
					steps = append(steps, appsql.Step{Operation: "exec", Context: ctx, SQL: q, Args: []any{`[]`}, Result: appsql.FakeResult{Affected: 1}}, appsql.Step{Operation: "commit"})
				}
				steps = append(steps, appsql.Step{Operation: "rollback"})
				f := &appsql.Fake{Steps: steps}
				s := &SQLite{db: &applicationDB{dialect: d, runner: f, transactions: f}}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != "edit panic" {
								t.Error("編集時のpanicが保持されていません")
							}
						}()
						_ = s.updateSessions(ctx, func([]sessionPair) ([]sessionPair, error) { panic("edit panic") })
					}()
				} else {
					err := s.RevokeSession(ctx, "missing")
					if mode == "empty" && err != nil {
						t.Fatal(err)
					}
					if mode == "empty-close-error" && !errors.Is(err, sentinel) {
						t.Fatal("空のRowsの解放エラーが保持されていません")
					}
				}
				if !rows.Closed {
					t.Fatal("Rowsが解放されていません")
				}
				if err := f.Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
