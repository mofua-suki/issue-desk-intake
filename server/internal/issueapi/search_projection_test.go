package issueapi

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/jobs"
)

func TestSearchReadsDispatchOnlyForPagePostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issues = []Object{clone(f.api.issue)}
	for i := 2; i <= 105; i++ {
		issue := clone(f.api.issue)
		issue["number"] = i
		f.api.issues = append(f.api.issues, issue)
	}
	if _, err := f.pg.Exec(`INSERT INTO jobs(repo,issue,title,kind,priority,filed_at,scope,scope_source,provider,tier,status,view,updated_at) SELECT repo,n,title,kind,priority,filed_at,scope,scope_source,provider,tier,'queued',view,updated_at FROM jobs CROSS JOIN generate_series(2,105) n WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := f.pg.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	stats := &measuredDB{}
	name := fmt.Sprintf("search_dispatch_%d", time.Now().UnixNano())
	sql.Register(name, measureDriver{f.pg.Driver(), stats})
	pg, err := sql.Open(name, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	j, err := jobs.New(pg, jobs.Options{AppDB: f.app.DB()})
	if err != nil {
		t.Fatal(err)
	}
	f.s.o.Postgres = pg
	f.s.o.Jobs = j
	// Warm the number and baseline records before measuring query counts.
	if _, err := f.s.search(f.ctx, Object{"countOnly": "1"}, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	counts := []int64{}
	for _, query := range []Object{{"countOnly": "1"}, {"limit": "1"}, {"limit": "100"}} {
		stats.calls.Store(0)
		result, err := f.s.search(f.ctx, query, auth.Principal{})
		if err != nil || result["total"] != 105 {
			t.Fatalf("query=%v result=%v err=%v", query, result, err)
		}
		counts = append(counts, stats.calls.Load())
	}
	if counts[1]-counts[0] != 1 || counts[2]-counts[0] != 100 {
		t.Fatalf("dispatch reads for count, one, hundred: %v", counts)
	}
}
