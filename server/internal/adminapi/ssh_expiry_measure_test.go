package adminapi

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type sshSQLTotals struct {
	Selects, Updates, Controls int
	Rows                       int64
}

func sshTotals(t *testing.T, records []sshSQLRecord) sshSQLTotals {
	t.Helper()
	var total sshSQLTotals
	for _, r := range records {
		if r.Err != nil {
			t.Fatalf("DB statement failed: %v", r.Err)
		}
		switch {
		case strings.HasPrefix(r.SQL, "SELECT"):
			total.Selects++
			total.Rows += r.Rows
		case strings.HasPrefix(r.SQL, "UPDATE"):
			total.Updates++
		case r.SQL == "begin" || r.SQL == "commit" || r.SQL == "rollback":
			total.Controls++
		default:
			t.Fatalf("unexpected measured SQL: %s", r.SQL)
		}
	}
	return total
}
func TestSSHListExpiresApprovedTicketsWithOneUpdate(t *testing.T) {
	f := repairFixture(t)
	want := map[string]string{
		insertSSHTicket(t, f, "approved", f.now.Add(-6*time.Minute)): "expired",
		insertSSHTicket(t, f, "approved", f.now):                     "approved",
		insertSSHTicket(t, f, "reported", f.now.Add(-6*time.Minute)): "reported",
	}
	trace := &sshSQLTrace{}
	f.pg = sshObservedDB(t, f, trace)
	f.restart()
	trace.take()

	code, value := f.call("GET", "/api/ssh-tickets", "screen", nil)
	status(t, code, 200, value)
	if total := sshTotals(t, trace.take()); total != (sshSQLTotals{Selects: 1, Updates: 1, Controls: 4, Rows: 3}) {
		t.Fatalf("HTTP database calls=%+v", total)
	}
	rows, ok := obj(value)["tickets"].([]any)
	if !ok || len(rows) != len(want) {
		t.Fatalf("tickets=%#v", obj(value)["tickets"])
	}
	got := make(map[string]string, len(rows))
	for _, raw := range rows {
		row := obj(raw)
		id, _ := row["id"].(string)
		state, _ := row["status"].(string)
		got[id] = state
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ticket states=%v, want %v", got, want)
	}
}
