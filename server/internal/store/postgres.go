package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func OpenPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// HealthChecker owns a connection independent of the business pool.
type HealthChecker struct {
	dsn     string
	mu      sync.Mutex
	conn    *pgx.Conn
	flight  *healthFlight
	connect func(context.Context, string) (*pgx.Conn, error)
	result  error
	checked time.Time
	now     func() time.Time
	closed  bool
}

type healthFlight struct {
	done           chan struct{}
	err            error
	callerCanceled bool
}

func NewHealthChecker(dsn string) *HealthChecker {
	return &HealthChecker{dsn: dsn, now: time.Now, connect: pgx.Connect}
}

// Check shares an in-flight check and caches completed results for two seconds.
func (h *HealthChecker) Check(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return errors.New("health checker closed")
		}
		if h.flight != nil {
			flight := h.flight
			h.mu.Unlock()
			select {
			case <-flight.done:
				if flight.callerCanceled {
					continue
				}
				return flight.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !h.checked.IsZero() && h.now().Before(h.checked.Add(2*time.Second)) {
			err := h.result
			h.mu.Unlock()
			return err
		}
		h.flight = &healthFlight{done: make(chan struct{})}
		flight := h.flight
		h.mu.Unlock()

		checkctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		err := h.check(checkctx)
		cancel()
		callerCanceled := ctx.Err() != nil
		h.mu.Lock()
		h.result = err
		// A caller cancellation must not poison subsequent requests.
		if !callerCanceled {
			h.checked = h.now()
		} else {
			h.checked = time.Time{}
		}
		h.flight = nil
		flight.err = err
		flight.callerCanceled = callerCanceled
		close(flight.done)
		h.mu.Unlock()
		return err
	}
}

func (h *HealthChecker) check(ctx context.Context) error {
	if h.conn == nil || h.conn.IsClosed() {
		conn, err := h.connect(ctx, h.dsn)
		if err != nil {
			return err
		}
		h.conn = conn
	}
	var version int
	err := h.conn.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&version)
	if err == nil {
		var list []Migration
		list, err = Migrations()
		if err == nil && version < len(list) {
			err = errors.New("database schema is incomplete")
		}
	}
	if err != nil {
		closectx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = h.conn.Close(closectx)
		cancel()
		h.conn = nil
	}
	return err
}

// Close waits for an outstanding bounded check before releasing its connection.
func (h *HealthChecker) Close() {
	h.mu.Lock()
	h.closed = true
	flight := h.flight
	h.mu.Unlock()
	if flight != nil {
		<-flight.done
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = h.conn.Close(ctx)
		cancel()
		h.conn = nil
	}
}
