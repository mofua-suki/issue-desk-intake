package admincli

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"strings"
	"testing"
)

func TestUsageReportCLIUsesReadOnlySnapshots(t *testing.T) {
	local := usePostgresAppStore(t)
	db := pgFixture(t)
	args, _ := fixture(t)
	ctx := context.Background()
	var err error
	if _, err = local.DB().ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('private','{"project":"owner/app","issues":[{"number":1}],"body":"PRIVATE_REPORT_FIXTURE","consultationBudget":{"calls":1,"weighted":110,"unknownCalls":0}}')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err = db.QueryRow(`INSERT INTO jobs(repo,issue,pr,status,result,filed_at)VALUES('owner/app',1,10,'merged','{"mergeCommit":"abc"}','2026-10-31')RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO job_events(job_id,kind,at,data)VALUES($1,'state','2026-11-01T01:00:00Z','{"to":"merged"}')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,source,host,sender) VALUES('draft-usage','owner/app',NULL,'2026-10-31T01:00:00Z','{"repo":"owner/app","draft":"private","at":"2026-10-31T01:00:00Z","source":"intake","step":"受付","inputTokens":100,"outputTokens":10,"cacheReadTokens":0,"cacheWriteTokens":0}','intake','nas','nas')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO jobs(repo,issue,status) VALUES('owner/app',2,'stopped')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO usage_rows(key,repo,issue,at,json,host,sender) VALUES('detail','owner/app',2,'2026-11-01T01:00:00Z','{"repo":"owner/app","issue":2,"step":"実装","generation":1,"inputTokens":100,"cacheReadTokens":0,"cacheWriteTokens":0,"outputTokens":10}','nas','nas')`); err != nil {
		t.Fatal(err)
	}
	reportArgs := append(args, "usage-per-issue", "report", "--from", "2026-11-01", "--to", "2026-11-01", "--format", "json")
	out, _, err := command(t, reportArgs, "")
	if err != nil {
		t.Fatal(err)
	}
	verify := func(out string) {
		t.Helper()
		var value map[string]any
		if json.Unmarshal([]byte(out), &value) != nil || fleet.Number(fleet.Object(value["total"])["count"]) != 1 || strings.Contains(out, "PRIVATE_REPORT_FIXTURE") {
			t.Fatal("レポートの件数または秘密境界が不正")
		}
		issues := fleet.List(value["issues"])
		if len(issues) != 1 || fleet.Number(fleet.Object(fleet.Object(issues[0])["total"])["count"]) != 1 {
			t.Fatal("下書きに紐づく使用量が集計されていない")
		}
	}
	verify(out)
	var base map[string]any
	if err = json.Unmarshal([]byte(out), &base); err != nil || base["detail"] != nil {
		t.Fatal("既定出力に detail がある", err)
	}
	all, _, err := command(t, append(append([]string{}, reportArgs...), "--population", "all"), "")
	if err != nil {
		t.Fatal(err)
	}
	verify(all)
	var detailed map[string]any
	if err = json.Unmarshal([]byte(all), &detailed); err != nil {
		t.Fatal(err)
	}
	issues := fleet.List(fleet.Object(detailed["detail"])["issues"])
	found := false
	for _, item := range issues {
		issue := fleet.Object(item)
		if fleet.Number(issue["issue"]) == 2 && issue["merged"] == false && len(fleet.List(issue["breakdown"])) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("未マージの内訳がない", detailed)
	}
	delete(detailed, "detail")
	baseJSON, _ := json.Marshal(base)
	detailedJSON, _ := json.Marshal(detailed)
	if string(baseJSON) != string(detailedJSON) {
		t.Fatal("既存の出力が変化")
	}
	tableArgs := append(append([]string{}, args...), "usage-per-issue", "report", "--from", "2026-11-01", "--to", "2026-11-01", "--format", "table")
	table, _, err := command(t, tableArgs, "")
	if err != nil {
		t.Fatal(err)
	}
	allTable, _, err := command(t, append(append([]string{}, tableArgs...), "--population", "all"), "")
	if err != nil || table != allTable || strings.Contains(allTable, "PRIVATE_REPORT_FIXTURE") {
		t.Fatal("表の出力が変化", err)
	}
	bothArgs := append(append([]string{}, args...), "usage-per-issue", "report", "--from", "2026-11-01", "--to", "2026-11-01", "--format", "both", "--population", "all")
	both, _, err := command(t, bothArgs, "")
	if err != nil || both != table+all {
		t.Fatal("表と JSON の出力が不正", err)
	}
	if out, _, err = command(t, append(append([]string{}, reportArgs...), "--population", "invalid"), ""); err == nil || out != "" {
		t.Fatal("不正な population を受理")
	}
	for _, duplicate := range [][]string{{"--population", "all", "--population", "merged"}, {"--population=merged", "--population=all"}} {
		if out, _, err = command(t, append(append([]string{}, reportArgs...), duplicate...), ""); err == nil || out != "" {
			t.Fatal("重複した population を受理")
		}
	}
	for _, bad := range [][]string{{"--from", "2026-02-30", "--to", "2026-03-01"}, {"--from", "2026-01-01", "--to", "2026-04-03"}} {
		if out, _, err = command(t, append(args, append([]string{"usage-per-issue", "report"}, bad...)...), ""); err == nil || out != "" {
			t.Fatal("不正期間を受理")
		}
	}
	var count int
	if err = local.DB().QueryRow(`SELECT count(*) FROM drafts`).Scan(&count); err != nil || count != 1 {
		t.Fatal("レポートが下書きを変更")
	}
}

func TestUsageReportRejectsDuplicateFlagsBeforeOpeningDatabase(t *testing.T) {
	for _, name := range []string{"from", "to", "repo", "format", "population"} {
		for _, equals := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/space", true: "/equals"}[equals], func(t *testing.T) {
				values := map[string]string{"from": "2026-11-01", "to": "2026-11-01", "repo": "owner/app", "format": "json", "population": "all"}
				var args []string
				for _, key := range []string{"from", "to", "repo", "format", "population", name} {
					if equals {
						args = append(args, "--"+key+"="+values[key])
					} else {
						args = append(args, "--"+key, values[key])
					}
				}
				var out strings.Builder
				if err := usageReport(context.Background(), nil, nil, args, &out); err == nil || out.Len() != 0 {
					t.Fatalf("重複指定を拒否できません: %v", args)
				}
				// 設定ファイルやDBに到達する前に、実際の管理CLI入口でも拒否する。
				out.Reset()
				err := Run(context.Background(), append([]string{"--config", "/nonexistent/usage-report-config", "usage-per-issue", "report"}, args...), strings.NewReader(""), &out, &out)
				if err == nil || !strings.Contains(err.Error(), "--from YYYY-MM-DD") || out.Len() != 0 {
					t.Fatalf("管理CLI入口で重複を拒否できません: %v, %v", args, err)
				}
			})
		}
	}
}

func TestUsageTableMatchesOriginalFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/usage-table.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Report map[string]any
		Table  string
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	got := usageReportTable(f.Report)
	if got != f.Table {
		g, w := strings.Split(got, "\n"), strings.Split(f.Table, "\n")
		for i := range g {
			if i >= len(w) || g[i] != w[i] {
				t.Fatalf("行%d: got %s", i, g[i])
			}
		}
		t.Fatal("表の行数が異なる")
	}
}
