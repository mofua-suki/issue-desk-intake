package screenapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/testutil"
)

type heartbeatDriver struct{}
type heartbeatConn struct{}
type heartbeatRows struct{}

func init() { sql.Register("screen_assignment_heartbeat", heartbeatDriver{}) }

func (heartbeatDriver) Open(string) (driver.Conn, error)  { return heartbeatConn{}, nil }
func (heartbeatConn) Prepare(string) (driver.Stmt, error) { panic("unexpected prepare") }
func (heartbeatConn) Close() error                        { return nil }
func (heartbeatConn) Begin() (driver.Tx, error)           { panic("unexpected transaction") }
func (heartbeatConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != "SELECT heartbeat_at FROM hosts WHERE host=$1" {
		panic(query)
	}
	return heartbeatRows{}, nil
}
func (heartbeatRows) Columns() []string         { return []string{"heartbeat_at"} }
func (heartbeatRows) Close() error              { return nil }
func (heartbeatRows) Next([]driver.Value) error { return io.EOF }

func TestShippedAssignmentsMetaHosts(t *testing.T) {
	output := testutil.ShippedAssignments(t)
	db, err := sql.Open("screen_assignment_heartbeat", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{o: Options{Config: func() (Object, error) { return Object{"assignments": output}, nil }, Providers: func(context.Context) (any, error) { return []any{}, nil }, Postgres: db, Now: time.Now}}
	result, err := s.meta(context.Background(), "http://example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result["hosts"], output["hosts"]) {
		t.Fatalf("hosts: %v != %v", result["hosts"], output["hosts"])
	}
}
