// Package leadership elects the single intake writer with a PostgreSQL advisory
// lock held on a dedicated session. Standby processes retry without starting
// background business work; unsuccessful attempts close their sessions.
package leadership

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"issue-desk-intake/server/internal/contract"
)

const Key int64 = 20261012
const heldSQL = `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted AND classid=$1 AND objid=$2 AND objsubid=1`

type Options struct {
	Retry time.Duration
	Check time.Duration
	Log   func(string)
}
type Lease struct {
	conn        *pgx.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	done        chan struct{}
	releaseDone chan struct{}
	released    bool
	lost        chan string
}

func (l *Lease) Context() context.Context { return l.ctx }
func (l *Lease) Lost() <-chan string      { return l.lost }
func Try(ctx context.Context, dsn string, options Options) (*Lease, bool, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connectCtx, dsn)
	if err != nil {
		return nil, false, errors.New("書き手の共有DBへ接続できません")
	}
	var locked bool
	if err = conn.QueryRow(connectCtx, "SELECT pg_try_advisory_lock($1)", Key).Scan(&locked); err != nil {
		conn.Close(context.Background())
		return nil, false, errors.New("書き手のロックを取得できません")
	}
	if !locked {
		conn.Close(context.Background())
		return nil, false, nil
	}
	if options.Check <= 0 {
		options.Check = 10 * time.Second
	}
	lifecycle, stop := context.WithCancel(ctx)
	lease := &Lease{conn: conn, ctx: lifecycle, cancel: stop, done: make(chan struct{}), releaseDone: make(chan struct{}), lost: make(chan string, 1)}
	go lease.watch(options.Check)
	return lease, true, nil
}
func Wait(ctx context.Context, dsn string, options Options) (*Lease, error) {
	if options.Retry <= 0 {
		options.Retry = 2 * time.Second
	}
	logged := false
	for {
		lease, ok, err := Try(ctx, dsn, options)
		if ok {
			return lease, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !logged && options.Log != nil {
			if err != nil {
				options.Log("共有DBの書き手のロックを確認できません。待機を続けます。原因: " + contract.SafeCause(err))
			} else {
				options.Log("別の受付が書き手です。待機を続けます。")
			}
			logged = true
		}
		timer := time.NewTimer(options.Retry)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}
func (l *Lease) watch(interval time.Duration) {
	l.watchWithCheck(interval, func(ctx context.Context) (int, error) {
		var held int
		err := l.conn.QueryRow(ctx, heldSQL, int64(Key>>32), int64(Key&0xffffffff)).Scan(&held)
		return held, err
	})
}

func (l *Lease) watchWithCheck(interval time.Duration, check func(context.Context) (int, error)) {
	defer close(l.done)
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-timer.C:
			l.mu.Lock()
			if l.released {
				l.mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(l.ctx, 3*time.Second)
			held, err := check(ctx)
			cancel()
			if l.ctx.Err() != nil {
				l.mu.Unlock()
				return
			}
			if err != nil || held != 1 {
				l.cancel()
				select {
				case l.lost <- "書き手のロックを確認できなくなったため処理を停止します。":
				default:
				}
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
		}
	}
}

// Release relinquishes this lease and waits for its watcher to finish. It is
// safe to call concurrently and never attempts to acquire the lock again.
func (l *Lease) Release() {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		<-l.releaseDone
		return
	}
	l.released = true
	l.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = l.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", Key)
	_ = l.conn.Close(ctx)
	l.mu.Unlock()
	<-l.done
	close(l.releaseDone)
}
