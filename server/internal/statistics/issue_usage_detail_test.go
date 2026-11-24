package statistics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
)

func TestIssueUsageDetailFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/issue-usage-detail.json")
	if err != nil {
		t.Fatal(err)
	}
	var data ReworkRows
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, job := range data.Jobs {
		rows := []AccountRecord{}
		events := []AccountRecord{}
		for _, row := range data.Usage {
			if accountKey(row) == accountKey(job) {
				rows = append(rows, row)
			}
		}
		for _, event := range data.Events {
			if event["job_id"] == job["id"] {
				events = append(events, event)
			}
		}
		got := buildIssueUsageDetail(job, rows, 0, events, nil)
		tally := fleet.Object(got["limitTally"])
		expected := jobs.TallyUsage(rows, 0, &jobs.Job{Repo: fleet.Text(job["repo"]), Issue: int64(fleet.Number(job["issue"]))}, nil)
		if tally["measured"] != expected.Measured || tally["estimated"] != expected.Estimated || tally["missingRows"] != expected.MissingRows {
			t.Fatal(got)
		}
		switch int(fleet.Number(job["issue"])) {
		case 1:
			totals := fleet.Object(fleet.Object(fleet.List(got["breakdown"])[0])["usage"])
			if totals["inputTokens"] != float64(100) || totals["cacheReadTokens"] != float64(0) || totals["outputTokens"] != float64(10) || totals["weighted"] != float64(150) || totals["count"] != 1 {
				t.Fatal(totals)
			}
			if got["merged"] != true || tally["measured"] != float64(150) {
				t.Fatal(got)
			}
		case 2:
			if got["scope_source"] != "widened" || tally["measured"] != float64(150) || tally["estimated"] != float64(150) || tally["missingRows"] != 1 || tally["resolved"] != true || len(fleet.List(got["breakdown"])) != 2 {
				t.Fatal(got)
			}
		case 3:
			missing := 0
			unknown := false
			for _, item := range fleet.List(got["breakdown"]) {
				group := fleet.Object(item)
				if group["generation"] != nil {
					t.Fatal(group)
				}
				missing += group["missingRows"].(int)
				if group["step"] == "工程不明" && group["missingRows"] == 1 {
					unknown = true
				}
			}
			if got["status"] != "closed" || !unknown || missing != tally["missingRows"] || !reflect.DeepEqual(expected.UnresolvedSteps, []string{"工程不明"}) {
				t.Fatal(got)
			}
		case 4:
			if tally["resolved"] != false || fleet.Object(got["usageLimit"])["reason"] != "上限超過" {
				t.Fatal(got)
			}
		}
	}
}

func TestIssueUsageDetailPostgresUnmerged(t *testing.T) {
	db, ctx := testPostgres(t)
	local := pgtest.NewIsolated(t).DB
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,scope_source) VALUES('o/r',8,'stopped','widened')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES('detail','o/r',8,'2026-11-01T00:00:00Z','{"repo":"o/r","issue":8,"step":"実装","provider":"codex","inputTokens":100,"cacheReadTokens":0,"cacheWriteTokens":0,"outputTokens":10}','nas','nas')`)
	if err != nil {
		t.Fatal(err)
	}
	p := Period{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	base, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(base)
	b, _ := json.Marshal(plain)
	if string(a) != string(b) {
		t.Fatal("default report changed")
	}
	report, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	issues := fleet.List(fleet.Object(report["detail"])["issues"])
	if len(issues) != 1 || fleet.Object(issues[0])["status"] != "stopped" {
		t.Fatal(report)
	}
	delete(report, "detail")
	c, _ := json.Marshal(report)
	if string(a) != string(c) {
		t.Fatal("legacy fields changed")
	}
}

// TestIssueUsageDetailFixturePostgres compares both public readers with the
// original report builder on the same complete fixture and database snapshot.
func TestIssueUsageDetailFixturePostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	local := pgtest.NewIsolated(t).DB
	raw, err := os.ReadFile("testdata/issue-usage-detail.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture ReworkRows
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int64{}
	for _, job := range fixture.Jobs {
		var id int64
		var pr any
		result := `{}`
		if job["status"] == "merged" {
			pr = 10
			result = `{"mergeCommit":"abc"}`
		}
		err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,scope_source,pr,result,filed_at) VALUES($1,$2,$3,$4,$5,$6,'2026-10-31') RETURNING id`, job["repo"], int64(fleet.Number(job["issue"])), job["status"], job["scope_source"], pr, result).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids[int64(fleet.Number(job["id"]))] = id
		if job["status"] == "merged" {
			if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'state','2026-11-01T12:00:00Z','{"to":"merged"}')`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, row := range fixture.Usage {
		// The fixture predates database insertion and has no timestamp; place every
		// row inside the requested period and before the merge event.
		row["at"] = "2026-11-01T00:30:00Z"
		value, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,$2,$3,$4,$5,'nas','test')`, fmt.Sprintf("fixture-%d", i), row["repo"], int64(fleet.Number(row["issue"])), row["at"], string(value))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range fixture.Events {
		data, e := json.Marshal(event["data"])
		if e != nil {
			t.Fatal(e)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'implementation_usage_limit',$2,$3)`, ids[int64(fleet.Number(event["job_id"]))], event["at"], string(data))
		if err != nil {
			t.Fatal(err)
		}
	}
	// This draft is read for attribution but its content must never be returned.
	if _, err = local.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('secret','{"body":"PRIVATE_FIXTURE","issues":[{"project":"o/r","number":1}]}')`); err != nil {
		t.Fatal(err)
	}
	p := Period{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	// Build the legacy output from the existing population reader, independently
	// of ReadIssueUsageReportWith's option and report assembly.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := ReadReworkRows(ctx, tx, "", p)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	period := AccountRecord{"fromDate": p.From.In(JST).Format("2006-01-02"), "toDate": p.To.Add(-1).In(JST).Format("2006-01-02"), "from": ISO(p.From), "to": ISO(p.To)}
	legacy := BuildIssueUsageReport(data, nil, period, AccountRecord{"draftPolicy": "first-filed-issue", "splitRows": 0})
	plain, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	optional, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detailed, err := ReadIssueUsageReportWith(ctx, db, local, "", p, nil, IssueUsageOptions{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(detailed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "PRIVATE_FIXTURE") {
		t.Fatal("draft content leaked")
	}
	detail := fleet.Object(detailed["detail"])
	description := fleet.Text(detail["description"])
	if !strings.Contains(description, "送信前の手元の行") || !strings.Contains(description, "generation=null は不明") {
		t.Fatal(description)
	}
	issues := fleet.List(detail["issues"])
	if len(issues) != len(fixture.Jobs) {
		t.Fatalf("detail issues: %v", issues)
	}
	for _, v := range issues {
		issue := fleet.Object(v)
		number := int(fleet.Number(issue["issue"]))
		tally := fleet.Object(issue["limitTally"])
		switch number {
		case 1:
			if issue["merged"] != true || tally["measured"] != float64(150) {
				t.Fatal(issue)
			}
		case 2:
			if issue["status"] != "running" || issue["scope_source"] != "widened" || tally["estimated"] != float64(150) || tally["missingRows"] != 1 {
				t.Fatal(issue)
			}
		case 3:
			missing, unknown := 0, false
			for _, v := range fleet.List(issue["breakdown"]) {
				group := fleet.Object(v)
				if group["generation"] != nil {
					t.Fatal(group)
				}
				missing += group["missingRows"].(int)
				if group["step"] == "工程不明" && group["missingRows"] == 1 {
					unknown = true
				}
			}
			if issue["status"] != "closed" || !unknown || missing != tally["missingRows"] || !reflect.DeepEqual(tally["unresolvedSteps"], []string{"工程不明"}) {
				t.Fatal(issue)
			}
		case 4:
			if issue["status"] != "stopped" || fleet.Object(issue["usageLimit"])["reason"] != "上限超過" || tally["resolved"] != false {
				t.Fatal(issue)
			}
		default:
			t.Fatalf("unexpected issue: %v", issue)
		}
	}
	delete(detailed, "detail")
	for name, report := range map[string]AccountRecord{"default": plain, "empty option": optional, "detail legacy fields": detailed} {
		if !reflect.DeepEqual(normalized(legacy), normalized(report)) {
			t.Fatalf("%s: %s", name, jsonDifference(normalized(legacy), normalized(report), "report"))
		}
	}
}

// TestIssueUsageDetailBatchPostgres checks query growth and issue/event attribution together.
func TestIssueUsageDetailBatchPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	tracer := &reworkQueryTracer{}
	cfg.Tracer = tracer
	observed := stdlib.OpenDB(*cfg)
	defer observed.Close()
	p := Period{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	run := func() (AccountRecord, int) {
		t.Helper()
		tx, e := observed.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if e != nil {
			t.Fatal(e)
		}
		tracer.queries = nil
		result, e := readIssueUsageDetail(ctx, tx, "", p, nil, nil)
		if e != nil {
			t.Fatal(e)
		}
		count := len(tracer.queries)
		if e = tx.Rollback(); e != nil {
			t.Fatal(e)
		}
		return result, count
	}
	insert := func(repo string, issue int) {
		t.Helper()
		var id int64
		if e := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status) VALUES($1,$2,'stopped') RETURNING id`, repo, issue).Scan(&id); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(AccountRecord{"repo": repo, "issue": issue, "step": "実装", "provider": "codex", "inputTokens": 100, "cacheReadTokens": 0, "cacheWriteTokens": 0, "outputTokens": 10})
		if _, e := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES($1,$2,$3,'2026-11-01T12:00:00Z',$4,'nas','nas')`, fmt.Sprintf("%s-%d", repo, issue), repo, issue, string(raw)); e != nil {
			t.Fatal(e)
		}
		if _, e := db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES($1,'implementation_usage_limit','2026-11-01','{"reason":"old"}'),($1,'implementation_usage_limit','2026-11-02','{"reason":"new"}')`, id); e != nil {
			t.Fatal(e)
		}
	}
	insert("a/r", 1)
	_, one := run()
	insert("b/r", 1)
	insert("a/r", 2)
	result, many := run()
	if one != many {
		t.Fatalf("queries grew: %d -> %d", one, many)
	}
	issues := fleet.List(result["issues"])
	if len(issues) != 3 {
		t.Fatal(result)
	}
	for _, v := range issues {
		item := fleet.Object(v)
		if fleet.Object(item["usageLimit"])["reason"] != "new" || fleet.Object(item["limitTally"])["measured"] != float64(150) {
			t.Fatal(item)
		}
	}
}
