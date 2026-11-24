package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
)

type canaryFixture struct {
	flavor        string
	db            *sql.DB
	service       *hostd.Service
	now           time.Time
	started       time.Time
	cycleAt       time.Time
	id            int64
	old, revision string
}

func newCanaryFixture(t *testing.T) *canaryFixture {
	return newCanaryFlavorFixture(t, "")
}
func newCanaryFlavorFixture(t *testing.T, flavor string) *canaryFixture {
	t.Helper()
	_, db, _ := workerFixture(t)
	f := &canaryFixture{flavor: flavor, db: db, now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), old: strings.Repeat("a", 40), revision: strings.Repeat("b", 40)}
	f.service = &hostd.Service{DB: db, Now: func() time.Time { return f.now }, Assignments: func() (*fleet.Assignments, error) {
		return &fleet.Assignments{ReleaseCanaryHosts: []string{"nas"}}, nil
	}}
	f.register(t, "*", f.old)
	f.exec(t, `INSERT INTO hosts(host,heartbeat_at,ready,image_revision,hostd_health) VALUES('nas',$1,true,$2,'{"cycle":{}}')`, f.now, f.revision[:12])
	f.exec(t, `INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES('nas',$1,$3,$2,'success')`, f.tag(f.old), "sha256:"+strings.Repeat("f", 64), f.flavor)
	f.register(t, "*", f.revision)
	if err := db.QueryRow(`SELECT id,started_at FROM release_rollouts WHERE status='canary'`).Scan(&f.id, &f.started); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *canaryFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *canaryFixture) tag(revision string) string {
	tag := revision[:12]
	if f.flavor == "-python" {
		tag += "-client-python"
	}
	return tag
}
func (f *canaryFixture) body(host, revision string) map[string]any {
	return map[string]any{"host": host, "flavor": f.flavor, "registry": "registry.example", "tag": f.tag(revision), "digest": "sha256:" + strings.Repeat("f", 64), "revision": revision}
}
func (f *canaryFixture) register(t *testing.T, host, revision string) {
	t.Helper()
	if _, err := f.service.Register(context.Background(), f.body(host, revision)); err != nil {
		t.Fatal(err)
	}
}
func (f *canaryFixture) tick(t *testing.T) {
	t.Helper()
	if err := f.service.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (f *canaryFixture) cycle(t *testing.T, id string) {
	t.Helper()
	if f.cycleAt.IsZero() {
		f.cycleAt = f.now.Add(-time.Minute)
	}
	f.cycleAt = f.cycleAt.Add(time.Second)
	sum := sha256.Sum256([]byte(id))
	cycle := map[string]any{"id": fmt.Sprintf("%x", sum[:16]), "phase": "finish", "result": "success", "stage": "complete", "exceptionType": nil, "at": f.cycleAt.UTC().Format(time.RFC3339Nano), "versions": map[string]any{"current": f.tag(f.revision), "previous": nil, "lastGood": nil}}
	if _, err := f.service.Health(context.Background(), "nas", map[string]any{"cycle": cycle}, true); err != nil {
		t.Fatal(err)
	}
}
func (f *canaryFixture) row(t *testing.T) string {
	t.Helper()
	var raw string
	if err := f.db.QueryRow(`SELECT row_to_json(r)::text FROM release_rollouts r WHERE id=$1`, f.id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
func (f *canaryFixture) want(t *testing.T, status, reason string, cycles int) {
	t.Helper()
	var gotStatus string
	var gotReason sql.NullString
	var gotCycles int
	var started time.Time
	if err := f.db.QueryRow(`SELECT status,reason,success_cycles,started_at FROM release_rollouts WHERE id=$1`, f.id).Scan(&gotStatus, &gotReason, &gotCycles, &started); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || !strings.HasPrefix(gotReason.String, reason) || gotCycles != cycles || !started.Equal(f.started) {
		t.Fatalf("status=%s reason=%s cycles=%d started=%v; want %s %s %d %v", gotStatus, gotReason.String, gotCycles, started, status, reason, cycles, f.started)
	}
}

func TestCanaryTickTimeoutIndependentOfDesired(t *testing.T) {
	for _, tc := range []struct {
		name                                                                string
		age                                                                 time.Duration
		override, missingHost, missingHeartbeat, changedGlobal, lateSuccess bool
		want                                                                string
	}{
		{name: "before_boundary", age: 180*time.Minute - time.Nanosecond, want: "canary"},
		{name: "boundary_host_override", age: 180 * time.Minute, override: true, want: "failed"},
		{name: "after_boundary", age: 181 * time.Minute, want: "failed"},
		{name: "missing_pc", age: 180 * time.Minute, missingHost: true, want: "failed"},
		{name: "missing_heartbeat", age: 180 * time.Minute, missingHeartbeat: true, want: "failed"},
		{name: "different_global", age: 180 * time.Minute, changedGlobal: true, want: "failed"},
		{name: "late_success_cannot_pass", age: 180 * time.Minute, lateSuccess: true, want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCanaryFixture(t)
			if tc.override {
				f.register(t, "nas", strings.Repeat("c", 40))
			}
			if tc.missingHost {
				f.exec(t, `DELETE FROM hosts WHERE host='nas'`)
			}
			if tc.missingHeartbeat {
				// Production forbids NULL heartbeat_at; cover defensive reads as well as a missing PC row.
				f.exec(t, `ALTER TABLE hosts ALTER COLUMN heartbeat_at DROP NOT NULL`)
				f.exec(t, `UPDATE hosts SET heartbeat_at=NULL WHERE host='nas'`)
			}
			if tc.changedGlobal {
				f.exec(t, `UPDATE releases SET revision=$1,tag=left($1,12) WHERE host='*'`, strings.Repeat("d", 40))
			}
			cycles := 0
			if tc.lateSuccess {
				f.cycle(t, "third")
				f.exec(t, `UPDATE release_rollouts SET success_cycles=2 WHERE id=$1`, f.id)
				cycles = 2
			}
			f.now = f.started.Add(tc.age)
			f.tick(t) // No Desired request, including during setup.
			reason := ""
			if tc.want == "failed" {
				reason = "timeout"
			}
			f.want(t, tc.want, reason, cycles)
			snapshot := f.row(t)
			var health string
			_ = f.db.QueryRow(`SELECT coalesce(hostd_health,'{}')::text FROM hosts WHERE host='nas'`).Scan(&health)
			f.tick(t)
			if snapshot != f.row(t) {
				t.Fatal("repeated Tick changed row")
			}
			var after string
			_ = f.db.QueryRow(`SELECT coalesce(hostd_health,'{}')::text FROM hosts WHERE host='nas'`).Scan(&after)
			if after != health {
				t.Fatal("repeated Tick changed commands")
			}
		})
	}
}

func TestCanaryTickLegacyNovemberSecondRow(t *testing.T) {
	f := newCanaryFixture(t)
	f.register(t, "nas", strings.Repeat("c", 40))
	f.started = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	f.exec(t, `UPDATE release_rollouts SET started_at=$2,success_cycles=0 WHERE id=$1`, f.id, f.started)
	f.tick(t)
	f.want(t, "failed", "timeout:host_override", 0)
}

func TestCanaryTickIncompatibleDesiredFailsWithinDeadline(t *testing.T) {
	f := newCanaryFixture(t)
	f.register(t, "nas", strings.Repeat("c", 40))
	f.tick(t)
	f.want(t, "failed", "desired_revision_mismatch:host_override", 0)
}

func TestCanaryTickDistinctCyclesUnlockGlobal(t *testing.T) {
	for _, flavor := range []string{"", "-python"} {
		t.Run("flavor"+flavor, func(t *testing.T) {
			for _, override := range []bool{false, true} {
				t.Run(fmt.Sprintf("host_override_%t", override), func(t *testing.T) {
					f := newCanaryFlavorFixture(t, flavor)
					if override {
						f.register(t, "nas", f.revision)
					}
					for i := 1; i <= 3; i++ {
						f.cycle(t, fmt.Sprint(i))
						f.tick(t)
						status, reason := "canary", ""
						if i == 3 {
							status, reason = "passed", "verified"
						}
						f.want(t, status, reason, i)
						f.tick(t)
						f.want(t, status, reason, i)
					}
					result, err := f.service.Desired(context.Background(), "other", flavor, f.old)
					if err != nil {
						t.Fatal(err)
					}
					if fleet.Object(result)["desired"].(*hostd.Release).Revision != f.revision {
						t.Fatal("global distribution remained held", result)
					}
				})
			}
		})
	}
}

func TestCanaryTickHealthConditions(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		args      []any
		reason    string
	}{
		{name: "stale_heartbeat", sql: `UPDATE hosts SET heartbeat_at=heartbeat_at-interval '11 minutes'`},
		{name: "future_heartbeat", sql: `UPDATE hosts SET heartbeat_at=heartbeat_at+interval '1 minute'`},
		{name: "missing_heartbeat", sql: `ALTER TABLE hosts ALTER COLUMN heartbeat_at DROP NOT NULL; UPDATE hosts SET heartbeat_at=NULL`},
		{name: "not_ready", sql: `UPDATE hosts SET ready=false`},
		{name: "wrong_image", sql: `UPDATE hosts SET image_revision='aaaaaaaaaaaa'`},
		{name: "drain", sql: `UPDATE hosts SET drain='{"hostd":{}}'`},
		{name: "legacy_drain", sql: `UPDATE hosts SET drain='{"by":"hostd:nas"}'`},
		{name: "image_failed", sql: `INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES('nas','bbbbbbbbbbbb','','sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff','failed')`, reason: "image_failed"},
		{name: "cycle_failed", sql: `UPDATE hosts SET hostd_health='{"cycle":{"id":"bad","phase":"finish","result":"failed","failures":2,"versions":{"current":"bbbbbbbbbbbb"}}}'`, reason: "cycle_failed"},
		{name: "waiting_is_not_failure", sql: `UPDATE hosts SET hostd_health='{"cycle":{"id":"wait","phase":"wait","result":"failed","failures":2,"versions":{"current":"bbbbbbbbbbbb"}}}'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCanaryFixture(t)
			f.cycle(t, "third")
			f.exec(t, `UPDATE release_rollouts SET success_cycles=2 WHERE id=$1`, f.id)
			f.exec(t, tc.sql, tc.args...)
			f.tick(t)
			status := "canary"
			if tc.reason != "" {
				status = "failed"
			}
			var gotStatus string
			var reason sql.NullString
			if err := f.db.QueryRow(`SELECT status,reason FROM release_rollouts WHERE id=$1`, f.id).Scan(&gotStatus, &reason); err != nil {
				t.Fatal(err)
			}
			if gotStatus != status || !strings.HasPrefix(reason.String, tc.reason) {
				t.Fatal(gotStatus, reason)
			}
		})
	}
}

func TestCanaryTickRollbackGuards(t *testing.T) {
	for _, tc := range []struct {
		name, skip string
		setup      func(*testing.T, *canaryFixture)
	}{
		{name: "issued"},
		{name: "host_override", skip: "host_override", setup: func(t *testing.T, f *canaryFixture) { f.register(t, "nas", f.revision) }},
		{name: "other_global", skip: "global_revision_changed", setup: func(t *testing.T, f *canaryFixture) {
			f.exec(t, `UPDATE releases SET revision=$1,tag=left($1,12) WHERE host='*'`, strings.Repeat("c", 40))
		}},
		{name: "pin", skip: "pinned", setup: func(t *testing.T, f *canaryFixture) {
			f.exec(t, `UPDATE hosts SET hostd_health=hostd_health||'{"commands":{"pin":{"tag":"old"}}}'`)
		}},
		{name: "pending", skip: "command_pending", setup: func(t *testing.T, f *canaryFixture) {
			raw := fmt.Sprintf(`{"commands":{"command":{"id":"pending","expiresAt":%d}}}`, f.now.Add(4*time.Hour).UnixMilli())
			f.exec(t, `UPDATE hosts SET hostd_health=hostd_health||$1::jsonb`, raw)
		}},
		{name: "not_switched", skip: "not_switched", setup: func(t *testing.T, f *canaryFixture) { f.exec(t, `UPDATE hosts SET hostd_health='{}'`) }},
		{name: "already_issued", skip: "already_issued", setup: func(t *testing.T, f *canaryFixture) {
			f.exec(t, `UPDATE release_rollouts SET rollback_command_id='existing' WHERE id=$1`, f.id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCanaryFixture(t)
			f.cycle(t, "one")
			if tc.setup != nil {
				tc.setup(t, f)
			}
			f.now = f.started.Add(180 * time.Minute)
			var before string
			if err := f.db.QueryRow(`SELECT hostd_health::text FROM hosts WHERE host='nas'`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			f.want(t, "failed", "timeout", 0)
			var reason string
			var id sql.NullString
			var after string
			if err := f.db.QueryRow(`SELECT reason,rollback_command_id FROM release_rollouts WHERE id=$1`, f.id).Scan(&reason, &id); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`SELECT hostd_health::text FROM hosts WHERE host='nas'`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if tc.skip != "" {
				if reason != "timeout:"+tc.skip || before != after {
					t.Fatal("rollback guard", reason, before, after)
				}
			} else {
				if !id.Valid || before == after {
					t.Fatal("rollback not issued", id, after)
				}
			}
			snapshot := f.row(t)
			f.tick(t)
			if snapshot != f.row(t) {
				t.Fatal("terminal row changed")
			}
			var repeated string
			_ = f.db.QueryRow(`SELECT hostd_health::text FROM hosts WHERE host='nas'`).Scan(&repeated)
			if repeated != after {
				t.Fatal("duplicate command")
			}
		})
	}
}

func TestCanaryTickDeadlinePrecedesFinalSuccess(t *testing.T) {
	for _, age := range []time.Duration{180*time.Minute - time.Nanosecond, 180 * time.Minute, 181 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			f := newCanaryFixture(t)
			f.now = f.started.Add(age)
			f.exec(t, `UPDATE hosts SET heartbeat_at=$1`, f.now)
			f.cycle(t, "third")
			f.exec(t, `UPDATE release_rollouts SET success_cycles=2,last_cycle_id='second' WHERE id=$1`, f.id)
			f.tick(t)
			if age < 180*time.Minute {
				f.want(t, "passed", "verified", 3)
			} else {
				f.want(t, "failed", "timeout", 2)
			}
		})
	}
}

func TestCanaryTickEvaluatesEverySavedID(t *testing.T) {
	f := newCanaryFixture(t)
	// Multiple pending rows, including an old global revision, must not be
	// collapsed to the latest row of a flavor during maintenance enumeration.
	f.exec(t, `INSERT INTO release_rollouts(host,flavor,revision,canary,status,previous,started_at) SELECT host,flavor,$2,canary,status,previous,started_at-interval '1 day' FROM release_rollouts WHERE id=$1`, f.id, strings.Repeat("c", 40))
	f.now = f.started.Add(180 * time.Minute)
	f.tick(t)
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM release_rollouts WHERE status='failed' AND reason LIKE 'timeout%'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestCanaryFiveMinuteCyclesObserveFailure(t *testing.T) {
	f := newCanaryFixture(t)
	report := func(id, result string) {
		t.Helper()
		cycle := map[string]any{"id": id, "phase": "finish", "result": result, "stage": "complete", "exceptionType": nil, "at": f.now.UTC().Format(time.RFC3339Nano), "versions": map[string]any{"current": f.tag(f.revision), "previous": nil, "lastGood": nil}}
		if _, err := f.service.Health(context.Background(), "nas", map[string]any{"cycle": cycle}, true); err != nil {
			t.Fatal(err)
		}
	}
	report("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "success")
	f.tick(t)
	f.want(t, "canary", "", 1)
	f.now = f.now.Add(5 * time.Minute)
	report("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "failed")
	f.now = f.now.Add(30 * time.Second)
	f.tick(t)
	f.want(t, "canary", "", 0)
	f.now = f.now.Add(4*time.Minute + 30*time.Second)
	report("cccccccccccccccccccccccccccccccc", "success")
	f.tick(t)
	f.want(t, "canary", "", 1)
}
