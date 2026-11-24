package statistics

import (
	"database/sql"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIssueUsageReadIncludesFirstDraftAndMergeCutoff(t *testing.T) {
	db, ctx := testPostgres(t)
	env := pgtest.NewIsolated(t)
	local := env.DB
	var e error
	if _, e = local.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('draft','{"project":"owner/repo","issues":[{"number":1},{"number":2}],"body":"PRIVATE_FIXTURE"}')`); e != nil {
		t.Fatal(e)
	}
	var id int64
	if e = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,pr,status,result,filed_at)VALUES('owner/repo',1,10,'merged','{"mergeCommit":"abc"}','2026-10-31')RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'state','2026-11-01T01:00:00Z','{"to":"merged"}')`, id); e != nil {
		t.Fatal(e)
	}
	for _, item := range []struct {
		key, at string
		issue   any
		draft   string
	}{{"before", "2026-10-30T00:00:00Z", nil, "draft"}, {"worker", "2026-11-01T00:00:00Z", int64(1), ""}, {"after", "2026-11-01T02:00:00Z", int64(1), ""}} {
		row := AccountRecord{"repo": "owner/repo", "issue": item.issue, "at": item.at, "draft": item.draft, "source": "intake", "step": "受付", "inputTokens": float64(100), "outputTokens": float64(10), "cacheReadTokens": float64(0), "cacheWriteTokens": float64(0), "elapsedMs": float64(5)}
		raw, _ := json.Marshal(row)
		if _, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,source,host,sender)VALUES($1,'owner/repo',$2,$3,$4,'intake','nas','nas')`, item.key, item.issue, item.at, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,source,host,sender)VALUES('broken-after','owner/repo',1,'2026-11-01T04:00:00Z','broken json','worker','nas','nas')`); e != nil {
		t.Fatal(e)
	}
	// A second connection to the same application schema that refuses writes.
	config, e := pgx.ParseConfig(env.DSN)
	if e != nil {
		t.Fatal("接続先を解釈できません")
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = env.Schema
	config.RuntimeParams["default_transaction_read_only"] = "on"
	ro := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	defer ro.Close()
	_, p, e := ReworkQuery(url.Values{"from": {"2026-11-01"}, "to": {"2026-11-01"}}, time.Now(), nil)
	if e != nil {
		t.Fatal(e)
	}
	report, e := ReadIssueUsageReportWith(ctx, db, ro, "", p, nil, IssueUsageOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if fleet.Number(report["skipped"]) != 0 {
		t.Fatal("マージ後の不正行を期間内欠測へ混ぜた")
	}
	issues := fleet.List(report["issues"])
	if len(issues) != 1 || fleet.Number(fleet.Object(fleet.Object(issues[0])["total"])["count"]) != 2 || fleet.Number(fleet.Object(report["attribution"])["splitRows"]) != 1 {
		t.Fatalf("attribution differs: %v", report)
	}
	raw, _ := json.Marshal(report)
	if string(raw) == "" || strings.Contains(string(raw), "PRIVATE_FIXTURE") {
		t.Fatal("draft body leaked")
	}
	if _, e = ro.ExecContext(ctx, `DELETE FROM drafts`); e == nil {
		t.Fatal("read-only application connection accepted mutation")
	}
}
