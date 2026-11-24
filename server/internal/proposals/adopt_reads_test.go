package proposals

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/consultations"
)

type proposalReadTracer struct{ full atomic.Int64 }

func (t *proposalReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT json FROM proposals ORDER BY") {
		t.full.Add(1)
	}
	return ctx
}
func (*proposalReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type permissionQueryTracer struct {
	mu    sync.Mutex
	query string
	args  []any
}

func (t *permissionQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT json FROM proposals WHERE ") {
		t.mu.Lock()
		t.query, t.args = data.SQL, append([]any(nil), data.Args...)
		t.mu.Unlock()
	}
	return ctx
}
func (*permissionQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestScanProposalReadsAndEarlierAdoption(t *testing.T) {
	s, api, _ := adoptFixture(t)
	var schema string
	if err := s.records.DB.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	tracer := &proposalReadTracer{}
	config.Tracer = tracer
	db := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	defer db.Close()
	s.records, err = consultations.OpenRecords(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	s.o.AppDB = db
	api.comments = append(api.comments, proposalComment(v3(), 11))
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatalf("同じ案が%d回起票されました", api.posts)
	}
	if got := tracer.full.Load(); got > 1 {
		t.Fatalf("proposals 全件読取: %d回", got)
	}
}

func TestScanRereadsDismissalAfterFilingWait(t *testing.T) {
	s, api, _ := adoptFixture(t)
	p := v3()
	p["id"] = "other-proposal"
	other := proposalComment(p, 11)
	api.comments = append(api.comments, other, proposalComment(p, 12))
	parsed, fp, err := parse(str(other["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reject(context.Background(), "owner/repo", 1, other, parsed, fp, "拒否"); err != nil {
		t.Fatal(err)
	}
	// The rejection is for comment 11; comment 12 is blocked by the dismissal.
	entered, release := make(chan struct{}), make(chan struct{})
	submit := s.o.Submit
	s.o.Submit = func(ctx context.Context, id string, d consultations.Record) (consultations.Record, error) {
		close(entered)
		<-release
		return submit(ctx, id, d)
	}
	done := make(chan error, 1)
	go func() { done <- s.Scan(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("起票待機に到達しません")
	}
	dismissed := make(chan error, 1)
	go func() {
		_, e := s.Dismiss(context.Background(), Object{"project": "owner/repo", "number": int64(1), "commentId": int64(11), "proposalId": p["id"], "fingerprint": fp, "reason": "不要", "id": "00000000-0000-4000-8000-000000000001"}, "screen", false)
		dismissed <- e
	}()
	select {
	case err := <-dismissed:
		if err != nil {
			close(release)
			<-done
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		<-done
		t.Fatal("破棄が待機に阻まれました")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 {
		t.Fatalf("破棄された案が起票されました: %d", api.posts)
	}
}

func TestScanFailsOnInvalidRejectionRecord(t *testing.T) {
	s, api, _ := adoptFixture(t)
	if _, err := s.records.DB.Exec("INSERT INTO proposals(id,json) VALUES($1,$2)", "rejection:owner/repo#1:99", "{"); err != nil {
		t.Fatal(err)
	}
	if err := s.Scan(context.Background()); err == nil {
		t.Fatal("不正な拒否記録を読み飛ばしました")
	}
	if api.posts != 0 {
		t.Fatal("読取失敗時に起票しました")
	}
}

func TestPermittedUsesLiteralRejectionPrefix(t *testing.T) {
	s, _, _ := adoptFixture(t)
	ctx := context.Background()
	p := v3()
	comment := proposalComment(p, 11)
	parsed, fp, err := parse(str(comment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated repository must not enter the selected prefix.
	if _, err := s.records.DB.ExecContext(ctx, `INSERT INTO proposals(id,json) VALUES($1,$2)`, "rejection:owner/repoX#1:11", "{"); err != nil {
		t.Fatal(err)
	}
	allowed, err := s.permitted(ctx, "owner/repo", 1, comment, parsed, fp)
	if err != nil || !allowed {
		t.Fatalf("別Issueの記録を読みました: allowed=%v err=%v", allowed, err)
	}
	if err := s.reject(ctx, "owner/repo", 1, comment, parsed, fp, "拒否"); err != nil {
		t.Fatal(err)
	}
	allowed, err = s.permitted(ctx, "owner/repo", 1, comment, parsed, fp)
	if err != nil || allowed {
		t.Fatalf("拒否記録が反映されません: allowed=%v err=%v", allowed, err)
	}
}

func TestPermissionLookupUsesIndex(t *testing.T) {
	s, _, _ := adoptFixture(t)
	var schema string
	if err := s.records.DB.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	tracer := &permissionQueryTracer{}
	config.Tracer = tracer
	db := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	defer db.Close()
	s.records, err = consultations.OpenRecords(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO proposals(id,json) SELECT 'rejection:other/repo#' || n || ':1', '{}' FROM generate_series(1, 20000) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE proposals`); err != nil {
		t.Fatal(err)
	}
	comment := proposalComment(v3(), 11)
	p, fp, err := parse(str(comment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := s.permitted(context.Background(), "owner/repo", 1, comment, p, fp)
	if err != nil || !allowed {
		t.Fatalf("判定に失敗: allowed=%v err=%v", allowed, err)
	}
	tracer.mu.Lock()
	query, args := tracer.query, tracer.args
	tracer.mu.Unlock()
	if query == "" {
		t.Fatal("判定のSQLを取得できません")
	}
	rows, err := db.Query("EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "idx_proposals") {
		t.Fatalf("拒否記録の範囲検索に索引が使われません: %s", plan.String())
	}
}
