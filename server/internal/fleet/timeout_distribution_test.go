package fleet

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Only the two read-only queries used by ClientConfig are needed here. The
// production fleet and assignments files still drive the entire config path.
type emptyConfigDB struct{ queries *unexpectedQueries }
type emptyConfigConn struct{ queries *unexpectedQueries }
type emptyConfigRows struct{}

type unexpectedQueries struct {
	mu      sync.Mutex
	queries []string
}

func (q *unexpectedQueries) record(query string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, query)
}

func (q *unexpectedQueries) snapshot() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.queries...)
}

func (db emptyConfigDB) Open(string) (driver.Conn, error)             { return emptyConfigConn(db), nil }
func (db emptyConfigDB) Connect(context.Context) (driver.Conn, error) { return db.Open("") }
func (db emptyConfigDB) Driver() driver.Driver                        { return db }
func (conn emptyConfigConn) Prepare(query string) (driver.Stmt, error) {
	conn.queries.record("prepare: " + query)
	return nil, fmt.Errorf("unexpected prepare: %s", query)
}
func (emptyConfigConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (emptyConfigConn) Close() error                             { return nil }
func (conn emptyConfigConn) Begin() (driver.Tx, error) {
	conn.queries.record("transaction: BEGIN")
	return nil, fmt.Errorf("unexpected transaction: BEGIN")
}
func (conn emptyConfigConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	fields := strings.Fields(strings.ToUpper(query))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "FROM" && (fields[i+1] == "SETTINGS" || fields[i+1] == "REPO_NOTE_PROCESSING") {
			return emptyConfigRows{}, nil
		}
	}
	conn.queries.record(query)
	return nil, fmt.Errorf("unexpected query: %s", query)
}
func (emptyConfigRows) Columns() []string         { return nil }
func (emptyConfigRows) Close() error              { return nil }
func (emptyConfigRows) Next([]driver.Value) error { return io.EOF }

func TestEmptyConfigDBQueriesAndUnsupportedOperations(t *testing.T) {
	queries := &unexpectedQueries{}
	conn := emptyConfigConn{queries}
	for _, query := range []string{
		"SELECT extra, key\nFROM\tsettings WHERE key = $1",
		"select repo, notes, extra from\nrepo_note_processing",
	} {
		rows, err := conn.QueryContext(context.Background(), query, nil)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if err := rows.Next(nil); err != io.EOF {
			t.Errorf("query %q: expected empty rows, got %v", query, err)
		}
		rows.Close()
	}
	query := "SELECT * FROM repo_note_processing_new"
	if _, err := conn.QueryContext(context.Background(), query, nil); err == nil || !strings.Contains(err.Error(), query) {
		t.Errorf("unexpected query error = %v", err)
	}
	if got := queries.snapshot(); len(got) != 1 || got[0] != query {
		t.Errorf("recorded unexpected queries = %q, want [%q]", got, query)
	}
	if _, err := conn.Prepare(query); err == nil || !strings.Contains(err.Error(), query) {
		t.Errorf("unexpected prepare error = %v", err)
	}
	if _, err := conn.Begin(); err == nil || !strings.Contains(err.Error(), "BEGIN") {
		t.Errorf("unexpected begin error = %v", err)
	}
	if got, want := queries.snapshot(), []string{query, "prepare: " + query, "transaction: BEGIN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded unexpected operations = %q, want %q", got, want)
	}

	db := sql.OpenDB(emptyConfigDB{queries})
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), query); err == nil {
		t.Error("unexpected exec succeeded")
	}
	if got, want := queries.snapshot(), []string{query, "prepare: " + query, "transaction: BEGIN", "prepare: " + query}; !reflect.DeepEqual(got, want) {
		t.Errorf("recorded unexpected operations after exec = %q, want %q", got, want)
	}
}

func TestDeployedFleetDistributesJobTimeoutToEveryHost(t *testing.T) {
	queries := &unexpectedQueries{}
	t.Cleanup(func() {
		if got := queries.snapshot(); len(got) != 0 {
			t.Errorf("unexpected queries: %q", got)
		}
	})
	db := sql.OpenDB(emptyConfigDB{queries})
	defer db.Close()
	root := filepath.Join("..", "..", "..")
	s := Service{DB: db, FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	doc, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Hosts) == 0 {
		t.Fatal("配布対象のPCがありません")
	}
	for host := range doc.Hosts {
		t.Run(host, func(t *testing.T) {
			body, err := s.ClientConfig(context.Background(), host, "")
			if err != nil {
				t.Fatal(err)
			}
			config := Object(body["config"])
			if Number(config["jobTimeoutSeconds"]) != 3600 {
				t.Fatalf("jobTimeoutSeconds = %v", config["jobTimeoutSeconds"])
			}
			lease := Number(Object(assignments.Output["dispatch"])["leaseMinutes"])
			if lease <= 0 || Number(Object(Object(config["assignments"])["dispatch"])["leaseMinutes"]) != lease {
				t.Fatal("lease設定が配布されていません")
			}
		})
	}
}
