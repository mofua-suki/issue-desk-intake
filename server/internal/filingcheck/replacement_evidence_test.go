package filingcheck

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

// The job store reads its normal SQL results from this isolated fixture.
type replacementConnector struct{}
type replacementConn struct{}
type replacementTx struct{}
type replacementRows struct{ values [][]driver.Value }

type draftConnector struct{ state *string }
type draftConn struct{ state *string }
type draftRows struct {
	state *string
	read  bool
}

func (c draftConnector) Connect(context.Context) (driver.Conn, error) { return draftConn(c), nil }
func (draftConnector) Driver() driver.Driver                          { return replacementDriver{} }
func (draftConn) Prepare(string) (driver.Stmt, error)                 { return nil, driver.ErrSkip }
func (draftConn) Close() error                                        { return nil }
func (draftConn) Begin() (driver.Tx, error)                           { return replacementTx{}, nil }
func (draftConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return replacementTx{}, nil
}
func (r *draftRows) Columns() []string { return []string{"json"} }
func (r *draftRows) Close() error      { return nil }
func (r *draftRows) Next(dst []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dst[0] = *r.state
	return nil
}
func (c draftConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, "SELECT json FROM drafts ") {
		return &draftRows{state: c.state}, nil
	}
	return nil, errors.New("unexpected draft query")
}
func (c draftConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(query, "UPDATE drafts SET json=") && len(args) == 3 && args[2].Value == *c.state {
		*c.state = args[0].Value.(string)
		return driver.RowsAffected(1), nil
	}
	return nil, errors.New("unexpected draft update")
}

func (replacementConnector) Connect(context.Context) (driver.Conn, error) {
	return replacementConn{}, nil
}
func (replacementConnector) Driver() driver.Driver { return replacementDriver{} }

type replacementDriver struct{}

func (replacementDriver) Open(string) (driver.Conn, error)  { return replacementConn{}, nil }
func (replacementConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (replacementConn) Close() error                        { return nil }
func (replacementConn) Begin() (driver.Tx, error)           { return replacementTx{}, nil }
func (replacementConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return replacementTx{}, nil
}
func (replacementTx) Commit() error       { return nil }
func (replacementTx) Rollback() error     { return nil }
func (replacementRows) Columns() []string { return make([]string, 24) }
func (replacementRows) Close() error      { return nil }
func (r *replacementRows) Next(dst []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dst, r.values[0])
	r.values = r.values[1:]
	return nil
}
func (replacementConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "WHERE repo=$1 AND issue=$2") {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		return &replacementRows{values: [][]driver.Value{{int64(1), "owner/repo", int64(9), "元依頼", "normal", int64(0), now, []byte(`[]`), "", nil, nil, "closed", nil, int64(0), nil, nil, nil, nil, int64(0), nil, nil, []byte(`{}`), nil, now}}}, nil
	}
	if strings.Contains(query, "FROM jobs WHERE lower(repo)") {
		return &replacementRows{}, nil
	}
	return nil, errors.New("unexpected job query")
}

func TestReplacementEvidence(t *testing.T) {
	mode := "ok"
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/api/v1/repos/owner/repo/issues" {
			_ = json.NewEncoder(w).Encode([]any{Record{"number": 9, "state": "open", "title": "元依頼", "body": "## 第一\n選択本文\n## 目的\n非選択本文", "user": Record{}}})
			return
		}
		if mode == "failure" {
			http.Error(w, "unavailable", 500)
			return
		}
		issue := Record{"number": 9, "state": "closed", "body": "## 第一\n選択本文\n## 目的\n非選択本文\n末尾"}
		if mode == "large" {
			issue["body"] = strings.Repeat("x", 61000)
		}
		if mode == "many" {
			issue["body"] = strings.Repeat("line\n", 30)
		}
		if mode == "pr" {
			issue["pull_request"] = Record{}
		}
		if mode == "wrong" {
			issue["number"] = 10
		}
		_ = json.NewEncoder(w).Encode(issue)
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(srv.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(replacementConnector{})
	defer db.Close()
	queue, err := jobs.New(db, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{Gitea: client, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }}}
	d := Record{"project": "owner/repo", "replaceIssues": []any{9}}
	material, err := s.materialWithDrafts(t.Context(), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := fleet.Elements(material["replacements"])
	if len(rows) != 1 || obj(rows[0])["candidateId"] != "request:9" || !hasFilingComparison(material) {
		t.Fatal(material)
	}
	if candidates(material)["request:9"] != nil || strings.Contains(string(raw(material)), "選択本文") {
		t.Fatal(material)
	}
	decision := duplicateVerdict(Record{"proceed": false, "duplicate": Record{"candidateId": "request:9"}}, material)
	if decision["proceed"] != true {
		t.Fatal(decision)
	}
	_, err = dependencies(Record{"dependsOnIssues": []any{Record{"number": 9}}, "dependencyEvidence": []any{Record{"candidateId": "request:9", "number": 9}}}, material)
	if err == nil {
		t.Fatal("置き換え元を依存候補として受理した")
	}
	request := func(id string, selection any) Record {
		return Record{"candidateId": id, "kind": "body", "selection": selection}
	}
	for _, selection := range []Record{{"body": Record{"section": "第一"}}, {"body": Record{"startLine": 2, "endLine": 2}}} {
		out, err := s.expand(t.Context(), d, material, []any{request("request:9", selection)})
		if err != nil || len(fleet.Elements(out["evidence"])) != 1 {
			t.Fatal(out, err)
		}
		body := fleet.Text(obj(fleet.Elements(out["evidence"])[0])["body"])
		if !strings.Contains(body, "選択本文") || strings.Contains(body, "非選択本文") {
			t.Fatal(body)
		}
	}
	calls = nil
	other := clone(d)
	other["project"] = "other/repo"
	for _, tc := range []struct {
		draft Record
		id    string
	}{{d, "request:10"}, {other, "request:9"}} {
		out, err := s.expand(t.Context(), tc.draft, material, []any{request(tc.id, nil)})
		if err != nil || len(fleet.Elements(out["evidenceOutsideCandidates"])) != 1 || len(fleet.Elements(out["evidence"])) != 0 {
			t.Fatal(out, err)
		}
	}
	if len(calls) != 0 {
		t.Fatal(calls)
	}
	for _, m := range []string{"failure", "pr", "wrong"} {
		mode = m
		out, err := s.expand(t.Context(), d, material, []any{request("request:9", nil)})
		if err != nil || len(fleet.Elements(out["evidenceMissing"])) != 1 || len(fleet.Elements(out["evidence"])) != 0 {
			t.Fatal(m, out, err)
		}
	}
	mode = "ok"
	out, err := s.expand(t.Context(), d, material, []any{request("request:9", Record{"body": Record{"section": "不存在"}})})
	if err != nil || len(fleet.Elements(out["evidenceMissing"])) != 1 || len(fleet.Elements(out["evidence"])) != 0 {
		t.Fatal(out, err)
	}
	mode = "many"
	wanted := []any{}
	for i := 1; i <= 25; i++ {
		wanted = append(wanted, request("request:9", Record{"body": Record{"startLine": i, "endLine": i}}))
	}
	out, err = s.expand(t.Context(), d, material, wanted)
	if err != nil || len(fleet.Elements(out["evidence"])) != 24 || len(fleet.Elements(out["evidenceOmitted"])) != 1 {
		t.Fatal(out, err)
	}
	mode = "large"
	out, err = s.expand(t.Context(), d, material, []any{request("request:9", nil)})
	if err != nil || len(fleet.Elements(out["evidence"])) != 0 || len(fleet.Elements(out["evidenceOmitted"])) != 1 {
		t.Fatal(out, err)
	}
	for _, req := range [][]any{{request("request:10", nil)}, {request("request:9", nil)}} {
		expanded, err := s.expandEvidence(t.Context(), d, material, req, req, nil)
		if err != nil || len(expanded.missing)+len(expanded.previousMissing) == 0 || len(fleet.Elements(expanded.current["evidence"])) != 0 {
			t.Fatal(expanded, err)
		}
	}

	// Exercise Check, including its saved hold, with the material produced above.
	for _, tc := range []struct {
		name, mode, id string
		selection      any
	}{
		{"outside", "ok", "request:10", nil},
		{"fetch", "failure", "request:9", nil},
		{"pull_request", "pr", "request:9", nil},
		{"selection", "ok", "request:9", Record{"body": Record{"section": "不存在"}}},
		{"budget", "large", "request:9", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode = "ok"
			draft := clone(d)
			draft["id"], draft["status"], draft["revision"] = "draft", "submitting", 1
			state := string(raw(draft))
			draftDB := sql.OpenDB(draftConnector{state: &state})
			defer draftDB.Close()
			attempts := 0
			check := &Service{options: Options{Gitea: client, Records: &consultations.Records{DB: draftDB}, Now: time.Now,
				Material: func(ctx context.Context, draft Record) (Record, error) { return s.materialWithDrafts(ctx, draft, nil) },
				Answer: func(_ context.Context, in map[string]any) (map[string]any, error) {
					attempts++
					input := fleet.Text(in["input"])
					if attempts == 1 && (strings.Contains(input, "選択本文") || !strings.Contains(input, "request:9")) {
						t.Fatal(input)
					}
					answer := allowed()
					mode = tc.mode
					if attempts == 1 || tc.name == "budget" {
						answer["proceed"] = false
						answer["evidenceRequests"] = []any{request(tc.id, tc.selection)}
					}
					return answer, nil
				},
			}}
			err := check.Check(t.Context(), draft)
			var saved Record
			if e := json.Unmarshal([]byte(state), &saved); e != nil {
				t.Fatal(e)
			}
			calls := obj(saved["filingCheckCalls"])
			if err == nil || !strings.Contains(err.Error(), "資料不足") || calls["stopKind"] != "evidence_missing" || obj(calls["evidenceHold"]) == nil || attempts == 0 {
				t.Fatal(err, calls, attempts)
			}
		})
	}
	t.Run("read", func(t *testing.T) {
		mode = "ok"
		draft := clone(d)
		draft["id"], draft["status"], draft["revision"] = "draft", "submitting", 1
		state := string(raw(draft))
		draftDB := sql.OpenDB(draftConnector{state: &state})
		defer draftDB.Close()
		attempts := 0
		check := &Service{options: Options{Gitea: client, Records: &consultations.Records{DB: draftDB}, Now: time.Now,
			Material: func(ctx context.Context, draft Record) (Record, error) { return s.materialWithDrafts(ctx, draft, nil) },
			Answer: func(_ context.Context, in map[string]any) (map[string]any, error) {
				attempts++
				input := fleet.Text(in["input"])
				answer := allowed()
				if attempts == 1 {
					if strings.Contains(input, "選択本文") || !strings.Contains(input, "request:9") {
						t.Fatal(input)
					}
					answer["proceed"] = false
					answer["evidenceRequests"] = []any{request("request:9", Record{"body": Record{"section": "第一"}})}
				} else if !strings.Contains(input, "選択本文") || strings.Contains(input, "非選択本文") {
					t.Fatal(input)
				}
				return answer, nil
			},
		}}
		if err := check.Check(t.Context(), draft); err != nil || attempts != 2 {
			t.Fatal(err, attempts)
		}
	})
}
