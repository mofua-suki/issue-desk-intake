package adminapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/store"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func sshResetter(t *testing.T, db *sql.DB) func() {
	t.Helper()
	sshExec(t, db, "DROP TABLE IF EXISTS ssh_expiry_seed")
	sshExec(t, db, "CREATE TABLE ssh_expiry_seed AS TABLE ssh_tickets")
	return func() {
		sshExec(t, db, "TRUNCATE ssh_tickets")
		sshExec(t, db, "INSERT INTO ssh_tickets SELECT * FROM ssh_expiry_seed")
	}
}
func sshJSON(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestSSHExpiryTimestampCompatibility(t *testing.T) {
	f := repairFixture(t)
	base := f.now
	for _, ttl := range []int{1, 5, 60} {
		for _, nano := range []int{0, 1, 499999, 999999, 1000001} {
			t.Run(fmt.Sprintf("ttl%d-now%d", ttl, nano), func(t *testing.T) {
				sshExec(t, f.pg, "TRUNCATE ssh_tickets")
				now := base.Add(time.Duration(nano))
				cutoff := now.Add(-time.Duration(ttl) * time.Minute)
				for _, state := range []string{"requested", "approved", "denied", "issued", "reported", "expired"} {
					for _, delta := range []time.Duration{-time.Millisecond, -time.Microsecond, 0, time.Microsecond, 500 * time.Microsecond, time.Millisecond, time.Minute} {
						insertSSHTicket(t, f, state, cutoff.Add(delta))
					}
					insertSSHTicket(t, f, state, nil)
				}
				// Storage is timestamptz, not arbitrary text. Exercise its real pgx decoder.
				for _, special := range []string{"infinity", "-infinity", "0001-01-01 00:00:00+00 BC", "0002-01-01 00:00:00+00 BC", "4713-01-01 00:00:00+00 BC", "9999-12-31 23:59:59.999999+00", "10000-01-01 00:00:00+00", "294276-12-31 23:59:59+00", "2026-10-31 23:55:00.000999+05:45"} {
					id := insertSSHTicket(t, f, "approved", special)
					row, err := queryMap(t.Context(), f.pg, "SELECT decided_at FROM ssh_tickets WHERE id=$1", id)
					if err != nil {
						t.Fatalf("stored special timestamp read: %s: %v", special, err)
					}
					_, valid := moment(row["decided_at"])
					t.Logf("stored=%s legacy-read=%v parsed=%v", special, row["decided_at"], valid)
				}
				before := sshSnapshot(t, f.pg)
				if _, err := f.pg.Exec("UPDATE ssh_tickets SET decided_at='not-a-timestamp' WHERE status='approved'"); err == nil {
					t.Fatal("invalid timestamp was stored")
				}
				if got := sshSnapshot(t, f.pg); !reflect.DeepEqual(before, got) {
					t.Fatal("rejected storage changed data")
				}
				reset := sshResetter(t, f.pg)
				if err := runSSHExpiry(t, f.pg, now, ttl, true); err != nil {
					t.Fatal(err)
				}
				want := sshSnapshot(t, f.pg)
				// Verify all saved columns, not only a count or a copied predicate.
				reset()
				if err := runSSHExpiry(t, f.pg, now, ttl, false); err != nil {
					t.Fatal(err)
				}
				if got := sshSnapshot(t, f.pg); !reflect.DeepEqual(want, got) {
					t.Fatal("saved rows differ from legacy")
				}
				// Route uses current policy and exactly one request-local clock value.
				f.now = now
				raw, err := os.ReadFile(f.policy)
				if err != nil {
					t.Fatal(err)
				}
				var policy map[string]any
				if err = json.Unmarshal(raw, &policy); err != nil {
					t.Fatal(err)
				}
				policy["ttlMinutes"] = ttl
				raw, err = json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(f.policy, raw, 0600); err != nil {
					t.Fatal(err)
				}
				reset()
				calls := 0
				f.options.Now = func() time.Time { calls++; return now }
				f.restart()
				calls = 0
				code, got := f.call("GET", "/api/ssh-tickets", "screen", nil)
				status(t, code, 200, got)
				if calls != 1 {
					t.Fatalf("request clock calls=%d", calls)
				}
				views := []map[string]any{}
				for _, row := range want {
					views = append(views, sshView(row))
				}
				if !reflect.DeepEqual(obj(got)["tickets"], sshJSON(t, views)) {
					t.Fatal("HTTP response differs from legacy saved rows")
				}
				if !reflect.DeepEqual(want, sshSnapshot(t, f.pg)) {
					t.Fatal("route saved values differ")
				}
				sshExec(t, f.pg, "DROP TABLE ssh_expiry_seed")
			})
		}
	}
}

func TestSSHExpiryDurabilityAndRejectionOrder(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, credential string
		data                           any
		code                           int
		expires                        bool
	}{
		{"issue-conflict", "POST", "/ID/issue", "signer", map[string]any{}, 409, true},
		{"non-signer", "POST", "/ID/issue", "worker", map[string]any{}, 403, true},
		{"invalid-input", "POST", "", "worker", map[string]any{}, 400, true},
		{"invalid-json", "POST", "", "worker", "not-an-object", 400, true},
		{"result-owner", "POST", "/ID/result", "delegate", map[string]any{}, 403, true},
		{"detail", "GET", "/ID", "screen", nil, 200, true},
		{"method", "DELETE", "/ID", "screen", nil, 405, true},
		{"unknown-action", "POST", "/ID/unknown", "worker", map[string]any{}, 404, true},
		{"approve-conflict", "POST", "/ID/approve", "screen", map[string]any{}, 409, true},
		{"deny-conflict", "POST", "/ID/deny", "screen", map[string]any{}, 409, true},
		{"unauthenticated", "GET", "", "", nil, 401, false},
		{"approve-authority", "POST", "/ID/approve", "worker", map[string]any{}, 403, false},
		{"deny-authority", "POST", "/ID/deny", "delegate", map[string]any{}, 403, false},
		{"csrf", "POST", "/ID/approve", "screen", map[string]any{}, 403, false},
		{"bad-route", "GET", "/bad-id", "screen", nil, 404, false},
		{"bad-policy", "GET", "", "screen", nil, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := repairFixture(t)
			id := insertSSHTicket(t, f, "approved", f.now.Add(-5*time.Minute))
			trace := &sshSQLTrace{}
			separate := sshObservedDB(t, f, trace)
			if tc.name == "bad-policy" {
				if err := os.Remove(f.policy); err != nil {
					t.Fatal(err)
				}
			}
			headers := map[string]string{}
			if tc.name == "csrf" {
				headers["X-CSRF-Token"] = "invalid"
			}
			path := "/api/ssh-tickets" + strings.ReplaceAll(tc.path, "ID", id)
			code, value := f.call(tc.method, path, tc.credential, tc.data, headers)
			status(t, code, tc.code, value)
			var saved string
			if err := separate.QueryRow("SELECT status FROM ssh_tickets WHERE id=$1", id).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			want := "approved"
			if tc.expires {
				want = "expired"
			}
			if saved != want {
				t.Fatalf("separate connection status=%s want=%s", saved, want)
			}
		})
	}
}
func TestSSHExpiryUpdateFailureRollsBack(t *testing.T) {
	f := repairFixture(t)
	insertSSHTicket(t, f, "approved", f.now.Add(-time.Hour))
	insertSSHTicket(t, f, "approved", f.now.Add(-time.Hour))
	before := sshSnapshot(t, f.pg)
	sshExec(t, f.pg, "CREATE SEQUENCE ssh_expiry_update_attempts")
	sshExec(t, f.pg, `CREATE FUNCTION reject_ssh_expiry() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 IF nextval('ssh_expiry_update_attempts') % 2 = 0 THEN
 RAISE EXCEPTION 'synthetic update failure';
 END IF;
 RETURN NEW;
 END $$`)
	sshExec(t, f.pg, `CREATE TRIGGER reject_ssh_expiry BEFORE UPDATE ON ssh_tickets FOR EACH ROW EXECUTE FUNCTION reject_ssh_expiry()`)
	for index, legacy := range []bool{true, false} {
		if err := runSSHExpiry(t, f.pg, f.now, 5, legacy); err == nil {
			t.Fatal("failed expiry succeeded")
		}
		if !reflect.DeepEqual(before, sshSnapshot(t, f.pg)) {
			t.Fatal("failed expiry partially persisted")
		}
		var attempts int
		if err := f.pg.QueryRow("SELECT last_value FROM ssh_expiry_update_attempts").Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 2*(index+1) {
			t.Fatalf("did not fail after an earlier successful write: attempts=%d", attempts)
		}
	}
	code, value := f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 503, value)
	if !reflect.DeepEqual(before, sshSnapshot(t, f.pg)) {
		t.Fatal("API continued after failed expiry")
	}
}

func TestSSHExpiryIssueBoundaryCompatibility(t *testing.T) {
	f := repairFixture(t)
	base := f.now
	for _, nanos := range []int{0, 1, 500001, 999999} {
		for _, delta := range []time.Duration{-time.Microsecond, 0, time.Microsecond, 500 * time.Microsecond, time.Millisecond} {
			for _, approver := range []any{"screen:fixture", "grant:fixture", "policy:status", nil} {
				t.Run(fmt.Sprintf("now%d-delta%d-approver%v", nanos, delta, approver), func(t *testing.T) {
					f.now = base.Add(time.Duration(nanos))
					sshExec(t, f.pg, "TRUNCATE ssh_tickets")
					id := insertSSHTicket(t, f, "approved", f.now.Add(-5*time.Minute+delta))
					sshExec(t, f.pg, "UPDATE ssh_tickets SET approver=$1 WHERE id=$2", approver, id)
					reset := sshResetter(t, f.pg)
					if err := runSSHExpiry(t, f.pg, f.now, 5, true); err != nil {
						t.Fatal(err)
					}
					oldCode, oldValue := f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
					oldRows := sshSnapshot(t, f.pg)
					reset()
					code, value := f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
					if code != oldCode || !reflect.DeepEqual(oldValue, value) || !reflect.DeepEqual(oldRows, sshSnapshot(t, f.pg)) {
						t.Fatalf("issue differs from legacy: old=%d %v new=%d %v", oldCode, oldValue, code, value)
					}
					decided, err := queryMap(t.Context(), f.pg, "SELECT decided_at FROM ssh_tickets WHERE id=$1", id)
					if err != nil {
						t.Fatal(err)
					}
					instant, valid := moment(decided["decided_at"])
					want := 409
					if approver != nil && approver != "policy:status" && valid && instant.After(f.now.Add(-5*time.Minute)) {
						want = 200
					}
					status(t, code, want, value)
				})
			}
		}
	}
}

// Exercise the distributed policy itself, not just the fixture's five-minute
// policy. Only synthetic principals and rows are added to the isolated DB.
func TestSSHExpiryShippedPolicy(t *testing.T) {
	f := repairFixture(t)
	f.options.Config["hostSshPolicyFile"] = "../../../host-ssh.json"
	s := &service{Options: f.options}
	policy, err := s.repairPolicy()
	if err != nil {
		t.Fatal(err)
	}
	hosts, operations := []string{}, []string{}
	for host := range policy.Hosts {
		hosts = append(hosts, host)
	}
	for operation := range policy.Operations {
		operations = append(operations, operation)
	}
	sort.Strings(hosts)
	sort.Strings(operations)
	var host, operation string
	for _, candidate := range hosts {
		for _, op := range operations {
			if policy.allows(candidate, op) {
				host, operation = candidate, op
				break
			}
		}
		if host != "" {
			break
		}
	}
	if host == "" {
		t.Fatal("distributed SSH policy has no allowed host and operation")
	}
	syntheticToken := "idk_" + strings.Repeat("d", 43)
	if _, err = f.db.AddPCToken(t.Context(), store.PCTokenRecord{ID: randomID(), Name: policy.Signer.Token, Hash: auth.Hash(syntheticToken), IssuedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	f.token["shipped-signer"] = syntheticToken
	f.restart()
	var future string
	for _, delta := range []time.Duration{-time.Microsecond, 0, time.Millisecond} {
		id := insertSSHTicket(t, f, "approved", f.now.Add(-time.Duration(policy.TTL)*time.Minute+delta))
		sshExec(t, f.pg, "UPDATE ssh_tickets SET host=$1,operation=$2 WHERE id=$3", host, operation, id)
		if delta > 0 {
			future = id
		}
	}
	reset := sshResetter(t, f.pg)
	if err = runSSHExpiry(t, f.pg, f.now, policy.TTL, true); err != nil {
		t.Fatal(err)
	}
	expected := sshSnapshot(t, f.pg)
	reset()
	code, value := f.call("GET", "/api/ssh-tickets", "screen", nil)
	status(t, code, 200, value)
	if !reflect.DeepEqual(expected, sshSnapshot(t, f.pg)) {
		t.Fatal("distributed policy expiry differs from legacy")
	}
	code, value = f.call("POST", "/api/ssh-tickets/"+future+"/issue", "shipped-signer", map[string]any{})
	status(t, code, 200, value)
	ttl, ok := integer(obj(value)["ttlMinutes"])
	if !ok || ttl != int64(policy.TTL) || obj(value)["principal"] != policy.Hosts[host].Principal || obj(value)["forceCommand"] != policy.Operations[operation] || obj(value)["expires_at"] != iso(f.now.Add(time.Duration(policy.TTL)*time.Minute)) {
		t.Fatal("distributed policy issue response changed")
	}
}
