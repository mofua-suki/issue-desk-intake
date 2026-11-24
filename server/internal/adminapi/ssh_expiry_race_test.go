package adminapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type sshRaceResult struct {
	code int
	err  error
}

// Also observe completed requests while waiting for a synchronization point:
// a request that fails before acquiring its lock must fail the test, not hang it.
func awaitSSHStage[T any](t *testing.T, stage <-chan T, first, second <-chan sshRaceResult) T {
	t.Helper()
	select {
	case value := <-stage:
		return value
	case result := <-first:
		t.Fatalf("first request ended before synchronization: status=%d err=%v", result.code, result.err)
	case result := <-second:
		t.Fatalf("second request ended before synchronization: status=%d err=%v", result.code, result.err)
	case <-t.Context().Done():
		t.Fatal("test cancelled while awaiting synchronization")
	}
	var zero T
	return zero
}

// Wait for a real PostgreSQL lock dependency, not a scheduler delay. The Go test
// runner's deadline bounds failures; no measurement/success timing threshold.
func awaitSSHBlocked(t *testing.T, db *sql.DB, pid uint32, first, second <-chan sshRaceResult) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := db.QueryRowContext(t.Context(), "SELECT cardinality(pg_blocking_pids($1)) > 0", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case result := <-first:
			t.Fatalf("first request ended before lock wait: status=%d err=%v", result.code, result.err)
		case result := <-second:
			t.Fatalf("second request ended before lock wait: status=%d err=%v", result.code, result.err)
		case <-t.Context().Done():
			t.Fatal("test cancelled while awaiting row lock")
		}
	}
}
func sshOtherService(t *testing.T, f *fixture, db *sql.DB, now time.Time) *fixture {
	t.Helper()
	other := *f
	other.pg = db
	other.options.Now = func() time.Time { return now }
	other.restart()
	return &other
}
func assertSSHIssued(t *testing.T, f *fixture, id string, issued bool) {
	t.Helper()
	var state string
	var at, expires sql.NullTime
	if err := f.pg.QueryRow("SELECT status,issued_at,expires_at FROM ssh_tickets WHERE id=$1", id).Scan(&state, &at, &expires); err != nil {
		t.Fatal(err)
	}
	if issued {
		if state != "issued" || !at.Valid || !expires.Valid || !at.Time.Equal(f.now) || !expires.Time.Equal(f.now.Add(5*time.Minute)) {
			t.Fatalf("issued values: %s %v %v", state, at, expires)
		}
	} else if state != "expired" || at.Valid || expires.Valid {
		t.Fatalf("expired values: %s %v %v", state, at, expires)
	}
}
func TestSSHExpiryIndependentServiceRace(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		orders := []string{"issue-first", "expiry-first"}
		if !legacy {
			orders = append(orders, "double-issue")
		}
		for _, order := range orders {
			t.Run(fmt.Sprintf("legacy%v-%s", legacy, order), func(t *testing.T) {
				f := repairFixture(t)
				id := insertSSHTicket(t, f, "approved", f.now.Add(-5*time.Minute+time.Millisecond))
				aTrace, bTrace := &sshSQLTrace{}, &sshSQLTrace{}
				aDB, bDB := sshObservedDB(t, f, aTrace), sshObservedDB(t, f, bTrace)
				a := sshOtherService(t, f, aDB, f.now)
				bNow := f.now.Add(time.Millisecond) // equality is expired
				if order == "double-issue" {
					bNow = f.now
				}
				b := sshOtherService(t, f, bDB, bNow)
				locked := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce, syncOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				gate := func(_ *pgx.Conn, q string) {
					matches := strings.Contains(q, "FOR UPDATE")
					if order == "expiry-first" {
						matches = isSSHExpirySQL(q)
					}
					if matches {
						syncOnce.Do(func() { close(locked); <-release })
					}
				}
				waiting := make(chan uint32, 1)
				var waiterOnce sync.Once
				notice := func(c *pgx.Conn, q string) {
					matches := isSSHExpirySQL(q)
					if order != "issue-first" {
						matches = strings.Contains(q, "FOR UPDATE")
					}
					if matches {
						waiterOnce.Do(func() { waiting <- c.PgConn().PID() })
					}
				}
				issue := func(fx *fixture) sshRaceResult {
					code, value, err := fx.callResult("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
					ttl, ok := integer(obj(value)["ttlMinutes"])
					if err == nil && code == 200 && (!ok || ttl != 5 || obj(value)["principal"] != "issue-desk-repair-test-host") {
						err = fmt.Errorf("issue response changed: %v", value)
					}
					return sshRaceResult{code, err}
				}
				expire := func() sshRaceResult {
					if legacy {
						if err := runSSHExpiry(t, bDB, bNow, 5, true); err != nil {
							return sshRaceResult{503, err}
						}
						return sshRaceResult{code: 200}
					}
					code, _, err := b.callResult("GET", "/api/ssh-tickets", "screen", nil)
					return sshRaceResult{code, err}
				}
				first, second := make(chan sshRaceResult, 1), make(chan sshRaceResult, 1)
				if order == "expiry-first" {
					bTrace.end = gate
					aTrace.start = notice
					go func() { first <- expire() }()
					awaitSSHStage(t, locked, first, nil)
					go func() { second <- issue(a) }()
				} else {
					aTrace.end = gate
					bTrace.start = notice
					go func() { first <- issue(a) }()
					awaitSSHStage(t, locked, first, nil)
					if order == "double-issue" {
						go func() { second <- issue(b) }()
					} else {
						go func() { second <- expire() }()
					}
				}
				pid := awaitSSHStage(t, waiting, first, second)
				awaitSSHBlocked(t, f.pg, pid, first, second)
				unblock()
				c1, c2 := <-first, <-second
				if c1.err != nil || c2.err != nil {
					t.Fatalf("request errors: first=%v second=%v", c1.err, c2.err)
				}
				want2 := 200
				if order != "issue-first" {
					want2 = 409
				}
				if c1.code != 200 || c2.code != want2 {
					t.Fatalf("responses=%d,%d want=200,%d", c1.code, c2.code, want2)
				}
				assertSSHIssued(t, f, id, order != "expiry-first")
				// All goroutines have completed before changing trace callbacks/cleanup.
			})
		}
	}
}

func TestSSHExpirySameServiceRace(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired%v", expired), func(t *testing.T) {
			f := repairFixture(t)
			decided := f.now.Add(-5 * time.Minute)
			if !expired {
				decided = decided.Add(time.Millisecond)
			}
			id := insertSSHTicket(t, f, "approved", decided)
			trace := &sshSQLTrace{}
			observed := sshObservedDB(t, f, trace)
			s := &service{Options: f.options}
			s.Postgres = observed
			f.mux = http.NewServeMux()
			f.mux.HandleFunc("/api/ssh-tickets", s.serve)
			f.mux.HandleFunc("/api/ssh-tickets/", s.serve)
			locked, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			trace.end = func(_ *pgx.Conn, q string) {
				if strings.Contains(q, "FOR UPDATE") {
					once.Do(func() { close(locked); <-release })
				}
			}
			codes := make(chan sshRaceResult, 3)
			go func() {
				code, _, err := f.callResult("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
				codes <- sshRaceResult{code, err}
			}()
			awaitSSHStage(t, locked, codes, nil)
			if s.mu.TryLock() {
				s.mu.Unlock()
				t.Fatal("service mutex released during issuance")
			}
			started := make(chan struct{}, 2)
			go func() {
				started <- struct{}{}
				code, _, err := f.callResult("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
				codes <- sshRaceResult{code, err}
			}()
			go func() {
				started <- struct{}{}
				code, _, err := f.callResult("GET", "/api/ssh-tickets", "screen", nil)
				codes <- sshRaceResult{code, err}
			}()
			<-started
			<-started
			unblock()
			counts := map[int]int{}
			for i := 0; i < 3; i++ {
				result := <-codes
				if result.err != nil {
					t.Error(result.err)
				}
				counts[result.code]++
			}
			want := map[int]int{200: 2, 409: 1}
			if expired {
				want = map[int]int{200: 1, 409: 2}
			}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("responses=%v want=%v", counts, want)
			}
			assertSSHIssued(t, f, id, !expired)
		})
	}
}
