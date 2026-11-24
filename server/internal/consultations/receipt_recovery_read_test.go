package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"github.com/jackc/pgx/v5/pgconn"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

type recoveryQuery struct {
	query string
	args  []driver.NamedValue
}
type recoveryLog struct {
	mu              sync.Mutex
	queries         []recoveryQuery
	projectionError string
}
type recoveryConnector struct {
	base driver.Connector
	log  *recoveryLog
}

func (c recoveryConnector) Driver() driver.Driver { return c.base.Driver() }
func (c recoveryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recoveryConn{PostgresConn: conn.(appsql.PostgresConn), log: c.log}, nil
}

type recoveryConn struct {
	appsql.PostgresConn
	log *recoveryLog
}

func (c *recoveryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.log.mu.Lock()
	c.log.queries = append(c.log.queries, recoveryQuery{query, append([]driver.NamedValue(nil), args...)})
	code := c.log.projectionError
	c.log.mu.Unlock()
	if code != "" && strings.HasPrefix(query, "SELECT id, CASE") {
		return nil, &pgconn.PgError{Code: code}
	}
	return c.PostgresConn.QueryContext(ctx, query, args)
}

func TestReceiptRecoveryReadsOnlyThinkingDrafts(t *testing.T) {
	for _, code := range []string{"", "22P02", "22P05"} {
		t.Run("projection-"+code, func(t *testing.T) {
			ctx := context.Background()
			env := pgtest.NewIsolated(t)
			config, err := pgx.ParseConfig(env.DSN)
			if err != nil {
				t.Fatal(err)
			}
			config.RuntimeParams["search_path"] = env.Schema
			log := &recoveryLog{projectionError: code}
			db := sql.OpenDB(recoveryConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), log})
			defer db.Close()
			records, err := OpenRecords(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct{ id, status string }{{"waiting-z", "thinking"}, {"finished", "draft"}, {"waiting-a", "thinking"}, {"waiting-m", "thinking"}} {
				d := Record{"id": row.id, "status": row.status, "project": "owner/app", "messages": []any{Record{"role": "user", "text": "hello", "id": "message-" + row.id}}}
				if _, err := records.Change(ctx, "drafts", row.id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
					t.Fatal(err)
				}
			}
			service := &Service{options: Options{Records: records, Now: time.Now, Project: func(string) error { return nil }}}
			started := make(chan Record, 3)
			release := make(chan struct{})
			engine, err := NewEngine(ctx, EngineOptions{Service: service, Concurrency: 3, Turn: func(ctx context.Context, d Record) (Record, error) {
				started <- d
				select {
				case <-release:
				case <-ctx.Done():
				}
				return Record{"reply": "ok"}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			defer close(release)
			log.mu.Lock()
			log.queries = nil
			log.mu.Unlock()
			if err := engine.recoverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for i := 0; i < 3; i++ {
				select {
				case d := <-started:
					seen[text(d["id"])] = true
					if len(fleet.Items(d["messages"])) != 1 {
						t.Fatalf("messages lost: %v", d)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("thinking draft did not start")
				}
			}
			if !seen["waiting-z"] || !seen["waiting-a"] || !seen["waiting-m"] || engine.Busy("finished") {
				t.Fatalf("wrong resumed drafts: %v", seen)
			}
			log.mu.Lock()
			queries := append([]recoveryQuery(nil), log.queries...)
			log.mu.Unlock()
			selected, full := 0, 0
			var starts []string
			for _, q := range queries {
				if strings.HasPrefix(q.query, "SELECT id, json FROM drafts ORDER BY") {
					full++
				}
				if strings.HasPrefix(q.query, "SELECT json FROM drafts WHERE id IN (") {
					selected++
					got := []string{}
					for _, arg := range q.args {
						got = append(got, arg.Value.(string))
					}
					if !reflect.DeepEqual(got, []string{"waiting-m", "waiting-a", "waiting-z"}) {
						t.Fatalf("selected order: %v", got)
					}
				}
				if strings.HasPrefix(q.query, "SELECT json FROM intake_receipts WHERE sender=") {
					starts = append(starts, q.args[1].Value.(string))
				}
			}
			if selected != 1 {
				t.Fatalf("selected full reads=%d queries=%v", selected, queries)
			}
			wantFull := 0
			if code != "" {
				wantFull = 1
			}
			if full != wantFull {
				t.Fatalf("fallback full reads=%d want=%d", full, wantFull)
			}
			if !reflect.DeepEqual(starts, []string{"message-waiting-m", "message-waiting-a", "message-waiting-z"}) {
				t.Fatalf("resume submission order: %v", starts)
			}
			// A processing receipt can remain visible while its AI turn is in flight.
			if _, err := db.ExecContext(ctx, "INSERT INTO intake_receipts(id,sender,send_id,json) VALUES($1,$2,$3,$4)",
				"processing", "test", "processing", `{"id":"processing","draftId":"waiting-a","phase":"processing"}`); err != nil {
				t.Fatal(err)
			}
			log.mu.Lock()
			log.queries = nil
			log.mu.Unlock()
			for range 2 {
				if err := engine.recoverOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			log.mu.Lock()
			defer log.mu.Unlock()
			for _, q := range log.queries {
				if strings.HasPrefix(q.query, "SELECT json FROM drafts WHERE id") {
					t.Errorf("recovery reread a running draft: %v", q)
				}
			}

		})
	}
}
