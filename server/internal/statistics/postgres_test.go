package statistics

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testPostgres(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("tests require an explicitly named issue_desk_test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err = admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "issue_desk_test") {
		admin.Close()
		t.Fatal("test database verification failed")
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "statistics_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup failed: %v", err)
		}
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestPostgresStatisticsSnapshotsAndDeploymentDurations(t *testing.T) {
	db, ctx := testPostgres(t)
	var e error
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	var id int64
	e = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view)VALUES('o/r',1,'merged',$1,'light',jsonb_build_object('mergeCommit',$2::text),'{}')RETURNING id`, at, commit).Scan(&id)
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range []struct {
		Kind string
		At   time.Time
		Data string
	}{{"leased", at.Add(time.Minute), `{}`}, {"pr_created", at.Add(2 * time.Minute), `{}`}, {"state", at.Add(5 * time.Minute), `{"to":"merged"}`}} {
		if _, e = db.ExecContext(ctx, "INSERT INTO job_events(job_id,host,kind,at,data)VALUES($1,'nas',$2,$3,$4)", id, event.Kind, event.At, event.Data); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO app_deployments(repo,commit_sha,deploy_host,status,finished_at)VALUES('o/r',$1,'nas','deployed',$2)`, commit, at.Add(6*time.Minute)); e != nil {
		t.Fatal(e)
	}
	// 独立レビューは使用量の行の実行時間で測り、実行時間の無い行・0の行は欠測にする。
	for i, elapsed := range []string{`,"elapsedMs":60000`, ``, `,"elapsedMs":0`} {
		row := fmt.Sprintf(`{"at":%q,"source":"worker","repo":"o/r","issue":1,"step":"独立レビュー"%s}`, iso(at.Add(4*time.Minute)), elapsed)
		if _, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,issue,step,json,sender)VALUES($1,$2,'pc','worker','o/r',1,'独立レビュー',$3,'test')`, fmt.Sprint("review-", i), at.Add(4*time.Minute), row); e != nil {
			t.Fatal(e)
		}
	}
	drafts := []map[string]any{{"id": "draft", "project": "o/r", "createdAt": iso(at.Add(-time.Minute)), "issue": map[string]any{"project": "o/r", "number": float64(1)}, "tier": map[string]any{"level": "light", "model": "m", "source": "user", "history": []any{}}, "messages": []any{map[string]any{"role": "assistant"}}}}
	s := Service{DB: db, AppDB: pgtest.New(t), Now: func() time.Time { return at.Add(time.Hour) }, Drafts: func(context.Context) ([]map[string]any, error) { return drafts, nil }}
	value, e := s.Overview(ctx, url.Values{"range": {"today"}}, []string{"o/r"})
	if e != nil {
		t.Fatal(e)
	}
	durations := fleet.Object(value.(map[string]any)["durations"])
	if review := fleet.Object(durations["review"]); review["averageMs"] != float64(60000) || review["count"] != 1 || review["missing"] != 2 || fleet.Object(durations["mergedToDeployed"])["averageMs"] != float64(60000) {
		t.Fatal(value)
	}
	q := Query{Before: Period{at.Add(-24 * time.Hour), at}, After: Period{at, at.Add(24 * time.Hour)}}
	for _, name := range strings.Fields("workTypes sizeEstimates usageLimits inventory tiers followups consultationBudget inputBudget throughput waits stalls giteaReads filingCheck priorityReview usageReview antigravity issueBody reviewNotes testSelection") {
		if _, e := s.Saved(ctx, name, q); e != nil {
			t.Errorf("%s: %v", name, e)
		}
	}
	if _, e = s.Scope(ctx, q); e != nil {
		t.Fatal(e)
	}
}

// TestPostgresSelfDeploymentDurations measures the worker and the intake, which
// update themselves outside app_deployments.
func TestPostgresSelfDeploymentDurations(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const worker, intake = "mofua-suki/issue-desk-worker", "mofua-suki/issue-desk-intake"
	sha := func(c string) string { return strings.Repeat(c, 40) }
	for i, j := range []struct {
		repo, commit string
		merged       time.Duration
	}{{worker, sha("a"), time.Minute}, {worker, sha("b"), 2 * time.Minute}, {intake, sha("c"), 3 * time.Minute}, {intake, sha("d"), 20 * time.Minute}, {intake, sha("7"), -time.Minute}, {"o/none", sha("e"), time.Minute}} {
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view)VALUES($1,$4,'merged',$2,'light',jsonb_build_object('mergeCommit',$3::text),'{}')RETURNING id`, j.repo, at, j.commit, i+1).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data)VALUES($1,'pc','state',$2,'{"to":"merged"}')`, id, at.Add(j.merged)); err != nil {
			t.Fatal(err)
		}
	}
	// The worker image of the later merge "b" carries "a" too; the first PC that
	// applied it decides. A failed apply and an image without a merge record do not count.
	for _, h := range []struct {
		host, tag, status string
		received          time.Duration
	}{{"nas", sha("b")[:12] + "-client-python", "success", 10 * time.Minute}, {"pc", sha("b")[:12] + "-client-python", "success", 12 * time.Minute}, {"pc", sha("a")[:12] + "-client-python", "failed", 4 * time.Minute}, {"pc", sha("f")[:12] + "-client-python", "success", 30 * time.Second}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO host_image_history(host,tag,flavor,digest,status,received_at)VALUES($1,$2,'-python','sha256:test',$3,$4)`, h.host, h.tag, h.status, at.Add(h.received)); err != nil {
			t.Fatal(err)
		}
	}
	// The intake records began with "8" at the start. "7" merged before that and
	// cannot be measured even though "c" includes it. The first takeover by "c" at
	// 4 minutes was switched back to "8"; the retry at 5 minutes deployed it, 2
	// minutes after its merge. "d" is not deployed yet.
	for _, takeover := range []struct {
		revision string
		at       time.Duration
	}{{sha("8"), 0}, {sha("c"), 4 * time.Minute}, {sha("8"), 4*time.Minute + 30*time.Second}, {sha("c"), 5 * time.Minute}, {sha("c"), 6 * time.Minute}, {sha("9"), 30 * time.Minute}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO server_takeovers(revision,at)VALUES($1,$2)`, takeover.revision, at.Add(takeover.at)); err != nil {
			t.Fatal(err)
		}
	}
	s := Service{DB: db, AppDB: pgtest.New(t), Now: func() time.Time { return at.Add(time.Hour) }}
	repos := []string{worker, intake, "o/none"}
	for _, c := range []struct {
		project              string
		count, missing, excl int
		averageMs, medianMs  float64
	}{
		{"", 3, 1, 2, float64((9+8+2)*time.Minute/time.Millisecond) / 3, float64(8 * time.Minute / time.Millisecond)},
		{worker, 2, 0, 0, float64(17*time.Minute/time.Millisecond) / 2, float64(17*time.Minute/time.Millisecond) / 2},
		{intake, 1, 1, 1, float64(2 * time.Minute / time.Millisecond), float64(2 * time.Minute / time.Millisecond)},
	} {
		values := url.Values{"range": {"today"}}
		if c.project != "" {
			values.Set("project", c.project)
		}
		value, err := s.Overview(ctx, values, repos)
		if err != nil {
			t.Fatal(err)
		}
		got := fleet.Object(fleet.Object(value.(map[string]any)["durations"])["mergedToDeployed"])
		if got["count"] != c.count || got["missing"] != c.missing || got["excluded"] != c.excl || got["averageMs"] != c.averageMs || got["medianMs"] != c.medianMs {
			t.Fatalf("project %q: %v", c.project, got)
		}
	}
}
