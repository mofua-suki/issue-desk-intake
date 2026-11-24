package leadership

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"issue-desk-intake/server/internal/pgtest"
)

func TestWatchCancellationAndLoss(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelParent bool
		held         int
		err          error
		wantLost     bool
	}{
		{name: "parent canceled during query", cancelParent: true},
		{name: "query failure", err: errors.New("query failed"), wantLost: true},
		{name: "lock absent", wantLost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			l := &Lease{ctx: ctx, cancel: cancel, done: make(chan struct{}), lost: make(chan string, 1)}
			started := make(chan struct{})
			proceed := make(chan struct{})
			go l.watchWithCheck(time.Millisecond, func(queryCtx context.Context) (int, error) {
				close(started)
				<-proceed
				if tc.cancelParent {
					return 0, queryCtx.Err()
				}
				return tc.held, tc.err
			})
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("query did not start")
			}
			if tc.cancelParent {
				cancelParent()
			}
			close(proceed)
			select {
			case <-l.done:
			case <-time.After(3 * time.Second):
				t.Fatal("watcher did not stop")
			}
			if got := len(l.lost); (got == 1) != tc.wantLost {
				t.Fatalf("lost notifications = %d, want lost %t", got, tc.wantLost)
			}
			if l.ctx.Err() == nil {
				t.Fatal("business context remains active")
			}
		})
	}
}

func TestSingleWriterAndLostSession(t *testing.T) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用試験DBだけが使用可能")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serial, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer serial.Close(context.Background())
	if _, e = serial.Exec(ctx, "SELECT pg_advisory_lock(2026110145)"); e != nil {
		t.Fatal(e)
	}
	first, ok, err := Try(ctx, dsn, Options{Check: 10 * time.Millisecond})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	defer first.Release()
	second, ok, err := Try(ctx, dsn, Options{})
	if err != nil || ok || second != nil {
		t.Fatal("two writers elected", ok, err)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var terminated bool
	if err = admin.QueryRow(ctx, "SELECT pg_terminate_backend($1)", first.conn.PgConn().PID()).Scan(&terminated); err != nil || !terminated {
		t.Fatal(err)
	}
	select {
	case <-first.Lost():
	case <-ctx.Done():
		t.Fatal("lost session not observed")
	}
	if first.Context().Err() == nil {
		t.Fatal("business context remains active")
	}
	third, ok, err := Try(ctx, dsn, Options{})
	if err != nil || !ok {
		t.Fatal("standby did not become writer", err)
	}
	third.Release()
}

func TestExplicitReleaseConcurrentWatch(t *testing.T) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用試験DBだけが使用可能")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serial, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer serial.Close(context.Background())
	if _, err = serial.Exec(ctx, "SELECT pg_advisory_lock(2026110145)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		first, ok, err := Try(ctx, dsn, Options{Check: time.Millisecond})
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		other, ok, err := Try(ctx, dsn, Options{})
		if err != nil || ok || other != nil {
			t.Fatal("writer acquired before release", err)
		}
		// Observe a real watcher query on the lease session before racing release
		// against subsequent checks; a short interval alone does not prove it ran.
		for {
			var observed bool
			err = serial.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND query=$2)", first.conn.PgConn().PID(), heldSQL).Scan(&observed)
			if err != nil {
				t.Fatal(err)
			}
			if observed {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("watcher query not observed")
			case <-time.After(time.Millisecond):
			}
		}
		finished := make(chan struct{}, 8)
		for j := 0; j < cap(finished); j++ {
			go func() { first.Release(); finished <- struct{}{} }()
		}
		for j := 0; j < cap(finished); j++ {
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("release did not finish")
			}
		}
		select {
		case message := <-first.Lost():
			t.Fatalf("release reported loss: %s", message)
		default:
		}
		if first.Context().Err() == nil {
			t.Fatal("released context still active")
		}
		next, ok, err := Try(ctx, dsn, Options{})
		if err != nil || !ok {
			t.Fatal("writer did not acquire after release", err)
		}
		first.Release()
		other, ok, err = Try(ctx, dsn, Options{})
		if err != nil || ok || other != nil {
			t.Fatal("released lease reacquired or second writer elected", err)
		}
		next.Release()
	}
}

// integrationDB uses the same database-wide serialization lock as the other
// lease tests; the writer advisory key itself is not scoped to a schema.
func integrationDB(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用試験DBだけが使用可能")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err = conn.Exec(context.Background(), "SELECT pg_advisory_lock(2026110145)"); err != nil {
		t.Fatal(err)
	}
	return dsn, conn
}

func receive(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not complete")
	}
}

// Wait is the startup entry point: it must not acquire while another session
// holds the writer lock, and must acquire once that lock is released.
func TestWaitAcquiresAfterRelease(t *testing.T) {
	dsn, _ := integrationDB(t)
	held, ok, err := Try(context.Background(), dsn, Options{Check: 20 * time.Millisecond})
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	t.Cleanup(held.Release)
	type result struct {
		lease *Lease
		err   error
	}
	got := make(chan result, 1)
	go func() {
		lease, err := Wait(context.Background(), dsn, Options{Retry: 30 * time.Millisecond})
		got <- result{lease, err}
	}()
	select {
	case r := <-got:
		if r.lease != nil {
			r.lease.Release()
		}
		t.Fatal("Wait acquired while the lock was held", r.err)
	case <-time.After(150 * time.Millisecond):
	}
	held.Release()
	var r result
	select {
	case r = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not acquire after release")
	}
	if r.err != nil || r.lease == nil {
		t.Fatalf("Wait: %v", r.err)
	}
	defer r.lease.Release()
	other, ok, err := Try(context.Background(), dsn, Options{})
	if other != nil {
		other.Release()
	}
	if err != nil || ok {
		t.Fatalf("second writer acquired: %v %v", ok, err)
	}
}

func TestWaitCancellation(t *testing.T) {
	dsn, _ := integrationDB(t)
	held, ok, err := Try(context.Background(), dsn, Options{})
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	t.Cleanup(held.Release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	lease, err := Wait(ctx, dsn, Options{Retry: 20 * time.Millisecond})
	if lease != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, %v", lease, err)
	}
}

func TestWaitLeaseDetectsMissingDatabaseLock(t *testing.T) {
	dsn, _ := integrationDB(t)
	lease, err := Wait(context.Background(), dsn, Options{Check: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	lease.mu.Lock()
	var unlocked bool
	err = lease.conn.QueryRow(context.Background(), "SELECT pg_advisory_unlock($1)", Key).Scan(&unlocked)
	lease.mu.Unlock()
	if err != nil || !unlocked {
		t.Fatalf("unlock: %v %v", unlocked, err)
	}
	receive(t, lease.Context().Done())
	select {
	case <-lease.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("loss not reported")
	}
}

func TestParentCancellationInterruptsPostgresQuery(t *testing.T) {
	env := pgtest.NewIsolated(t)
	deadline, finish := context.WithTimeout(context.Background(), 15*time.Second)
	defer finish()
	conn, err := pgx.Connect(deadline, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	observer, err := pgx.Connect(deadline, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(context.Background())
	parent, cancel := context.WithCancel(deadline)
	defer cancel()
	lease := &Lease{ctx: parent, cancel: cancel, done: make(chan struct{}), lost: make(chan string, 1)}
	queryResult := make(chan error, 1)
	go lease.watchWithCheck(time.Millisecond, func(ctx context.Context) (int, error) {
		_, err := conn.Exec(ctx, "SELECT pg_sleep(30)")
		queryResult <- err
		return 1, err
	})
	defer func() { cancel(); <-lease.done }()
	for {
		var active bool
		err := observer.QueryRow(deadline, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND state='active' AND wait_event='PgSleep')", conn.PgConn().PID()).Scan(&active)
		if err != nil {
			t.Fatal(err)
		}
		if active {
			break
		}
		select {
		case <-deadline.Done():
			t.Fatal("blocking PostgreSQL query was not observed")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-queryResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pgx cancellation: %v", err)
		}
	case <-deadline.Done():
		t.Fatal("pgx query did not stop")
	}
	select {
	case <-lease.done:
	case <-deadline.Done():
		t.Fatal("watcher did not stop")
	}
	select {
	case reason := <-lease.Lost():
		t.Fatalf("parent cancellation reported lock loss: %s", reason)
	default:
	}
}
