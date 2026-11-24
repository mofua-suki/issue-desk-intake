package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Wait for PostgreSQL itself to report a blocked statement. This coordinates
// the real transaction boundaries rather than relying on scheduler sleeps.
func waitCanaryStatement(t *testing.T, f *canaryFixture, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := findBlockedCanaryStatement(ctx, f.db, prefix); err != nil {
		t.Fatal(err)
	}
}

func findBlockedCanaryStatement(ctx context.Context, db *sql.DB, prefix string) error {
	for {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a
			JOIN pg_locks l ON l.pid=a.pid
			JOIN pg_class c ON c.oid=l.relation
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE a.datname=current_database() AND a.wait_event_type='Lock'
			AND left(a.query, length($1))=$1 AND n.nspname=current_schema()
			AND c.relname IN ('releases','release_rollouts'))`, prefix).Scan(&blocked)
		if err != nil {
			return fmt.Errorf("waiting for blocked statement %q: %w", prefix, err)
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for blocked statement %q: %w", prefix, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCanaryStatementWaitExpires(t *testing.T) {
	f := newCanaryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := findBlockedCanaryStatement(ctx, f.db, "no such canary statement"); err == nil {
		t.Fatal("expected a timeout when Register is not blocked")
	}
}

func TestCanaryTickRechecksAfterRegistration(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "ended"
		if replace {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			f := newCanaryFixture(t)
			f.now = f.started.Add(181 * time.Minute)
			f.exec(t, `UPDATE hosts SET heartbeat_at=$1`, f.now)
			// Hold the row so Register takes the releases lock and waits for this row.
			blocker, err := f.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err = blocker.Exec(`SELECT id FROM release_rollouts WHERE id=$1 FOR UPDATE`, f.id); err != nil {
				t.Fatal(err)
			}
			registration := make(chan error, 1)
			revision := f.old
			if replace {
				revision = strings.Repeat("d", 40)
			}
			go func() { _, e := f.service.Register(context.Background(), f.body("*", revision)); registration <- e }()
			waitCanaryStatement(t, f, "SELECT id,revision,status,previous FROM release_rollouts")
			tick := make(chan error, 1)
			go func() { tick <- f.service.Tick(context.Background()) }()
			waitCanaryStatement(t, f, "LOCK TABLE releases IN SHARE ROW EXCLUSIVE MODE")
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = <-registration; err != nil {
				t.Fatal(err)
			}
			if err = <-tick; err != nil {
				t.Fatal(err)
			}
			f.want(t, "superseded", "", 0)
			snapshot := f.row(t)
			f.tick(t)
			if snapshot != f.row(t) {
				t.Fatal("ended/replaced row changed")
			}
			var count int
			if err = f.db.QueryRow(`SELECT count(*) FROM release_rollouts`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if replace {
				want = 2
			}
			if count != want {
				t.Fatal("replacement", count, want)
			}
		})
	}
}
