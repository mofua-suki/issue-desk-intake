package issueapi

import (
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/numbers"
	"reflect"
	"strings"
	"testing"
)

func TestIssueSummarySkipsCloseHistoryPostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issue["body"] = "本文の抜粋"
	f.api.comments = []Object{
		{"id": 1, "user": Object{"login": "intake"}, "body": "最初の質問 <!-- gcr:owner/repo#1:question -->"},
		{"id": 2, "user": Object{"login": "intake"}, "body": "最新の質問 <!-- gcr:owner/repo#1:question -->"},
	}
	if err := f.s.saveClose(f.ctx, Object{"id": "valid", "project": "owner/repo", "number": 1, "status": "closed", "reason": "完了", "pc": "nas", "decision": Object{"method": "manual"}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/issue?project=owner%2Frepo&number=1"
	status, value := f.call("GET", path, "screen", nil)
	full := fleet.ObjectOrEmpty(value)
	history := fleet.Items(full["closeHistory"])
	if status != 200 || len(history) != 1 || fleet.ObjectOrEmpty(history[0])["reason"] != "完了" || fleet.ObjectOrEmpty(history[0])["method"] != "manual" {
		t.Fatalf("full detail: status=%d value=%v", status, value)
	}
	status, value = f.call("GET", path+"&view=summary", "screen", nil)
	if status != 200 {
		t.Fatalf("initial summary: status=%d value=%v", status, value)
	}
	before := fleet.ObjectOrEmpty(value)
	if _, err := f.app.DB().ExecContext(f.ctx, "INSERT INTO close_requests(id,json) VALUES(?,?)", "broken", "[1]"); err != nil {
		t.Fatal(err)
	}
	status, value = f.call("GET", path+"&view=summary", "screen", nil)
	summary := fleet.ObjectOrEmpty(value)
	if status != 200 || summary["bodyExcerpt"] != "本文の抜粋" || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(summary["latestAsk"])["body"]), "最新の質問") || fleet.Text(summary["contentRevision"]) == "" || summary["contentRevision"] != before["contentRevision"] || summary["actions"] == nil || !reflect.DeepEqual(summary["actions"], full["actions"]) {
		t.Fatalf("summary: status=%d value=%v before=%v", status, value, before)
	}
	status, value = f.call("GET", path, "screen", nil)
	if status != 503 {
		t.Fatalf("broken history detail: status=%d value=%v", status, value)
	}
}

func TestDetailHTTPReusesNumberLookupsAcrossReferences(t *testing.T) {
	f := newFixture(t)
	db := sql.OpenDB(&lookupConnector{})
	defer db.Close()
	f.options.Numbers = &numbers.Store{DB: db}
	f.restart()
	f.api.issue["title"] = "#3 題名"
	f.api.issue["body"] = "元の依頼: #3\n<!-- issue-desk:followup -->"
	for i := 0; i < 20; i++ {
		f.api.comments = append(f.api.comments, Object{"id": i + 1, "body": "依存待機中\n#3: PRマージ待ち\n後続Issueを起票しました: #4\n<!-- gcr:owner/repo#1:question -->"})
	}
	for _, suffix := range []string{"", "&view=summary"} {
		lookupQueries = nil
		status, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1"+suffix, "nas", nil)
		if status != 200 {
			t.Fatalf("status=%d response=%v", status, value)
		}
		detail := fleet.ObjectOrEmpty(value)
		if !strings.Contains(fleet.Text(detail["shownTitle"]), "#90（Gitea番号 3）") {
			t.Fatalf("title: %v", detail)
		}
		if suffix == "" && (len(fleet.Items(detail["comments"])) != 20 || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(fleet.Items(detail["comments"])[0])["body"]), "Gitea番号 4")) {
			t.Fatalf("comments: %v", detail["comments"])
		}
		if suffix != "" && (detail["commentCount"] != float64(20) || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(detail["latestAsk"])["body"]), "Gitea番号 4")) {
			t.Fatalf("summary: %v", detail)
		}
		counts := map[string]int{}
		for _, q := range lookupQueries {
			counts[q]++
		}
		if counts["owner/repo#3"] != 1 || counts["owner/repo#4"] != 1 {
			t.Fatalf("number lookups: %v", lookupQueries)
		}
	}
}

func TestIssueSummaryOmitsHistoryButDetectsContentEdits(t *testing.T) {
	detail := Object{"project": "owner/repo", "number": float64(1), "shownBody": strings.Repeat("😀", 220) + "tail<!-- issue-desk:settings {} -->", "images": []any{}, "result": Object{"secret": "not part of summary"}, "comments": []any{Object{"id": float64(1), "kind": "ask", "body": "質問", "createdAt": "2026-11-01T00:00:00Z", "images": []any{}}, Object{"id": float64(2), "kind": "log", "body": "開始"}}, "actions": []any{"reply"}}
	first := summarizeDetail(detail)
	if len([]rune(fleet.Text(first["bodyExcerpt"]))) != 200 || first["bodyTruncated"] != true || first["commentCount"] != 1 || fleet.ObjectOrEmpty(first["latestAsk"])["body"] != "質問" || first["body"] != nil || first["shownBody"] != nil || first["comments"] != nil || first["images"] != nil || first["result"] != nil {
		t.Fatalf("summary %v", first)
	}
	fleet.ObjectOrEmpty(fleet.Items(detail["comments"])[1])["body"] = "別の経過"
	if summarizeDetail(detail)["contentRevision"] != first["contentRevision"] {
		t.Fatal("progress log changed the content revision")
	}
	detail["shownBody"] = fleet.Text(detail["shownBody"]) + "changed tail"
	if summarizeDetail(detail)["contentRevision"] == first["contentRevision"] {
		t.Fatal("body-tail edit was hidden by the unchanged excerpt")
	}
	detail["shownBody"] = strings.Repeat("😀", 220) + "tail<!-- issue-desk:settings {} -->"
	fleet.ObjectOrEmpty(fleet.Items(detail["comments"])[0])["body"] = "編集された質問"
	if summarizeDetail(detail)["contentRevision"] == first["contentRevision"] {
		t.Fatal("existing question edit was not detected")
	}
}

func TestDetailRepeatedReferencesSummaryAndFullPostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issue["title"] = "#1 題名"
	f.api.issue["body"] = "元の依頼: #1\n<!-- issue-desk:followup -->"
	for i := 0; i < 100; i++ {
		kind := "question"
		if i == 0 {
			kind = "start"
		}
		f.api.comments = append(f.api.comments, Object{"id": i + 1, "user": Object{"login": "intake"}, "body": "依存待機中\n#1: PRマージ待ち\n後続Issueを起票しました: #99\n<!-- gcr:owner/repo#1:" + kind + " -->"})
	}
	full, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	comments := fleet.Items(full["comments"])
	if len(comments) != 100 || fleet.ObjectOrEmpty(comments[0])["kind"] != "log" || fleet.ObjectOrEmpty(comments[1])["kind"] != "ask" || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(comments[1])["body"]), fmt.Sprintf("#%v（Gitea番号 1）", full["desk"])) || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(comments[1])["body"]), "Gitea番号 99") || full["body"] != f.api.issue["body"] {
		t.Fatal(full)
	}
	summary, err := f.s.Detail(f.ctx, "owner/repo", 1, false, true)
	if err != nil {
		t.Fatal(err)
	}
	expected := summarizeDetail(full)
	if summary["contentRevision"] != expected["contentRevision"] || summary["commentCount"] != 99 || fleet.ObjectOrEmpty(summary["latestAsk"])["body"] != fleet.ObjectOrEmpty(comments[99])["body"] {
		t.Fatal(summary, expected)
	}
	f.api.comments[1]["body"] = "変更 <!-- gcr:owner/repo#1:question -->"
	changed, err := f.s.Detail(f.ctx, "owner/repo", 1, false, true)
	if err != nil || changed["contentRevision"] == summary["contentRevision"] {
		t.Fatal(changed, err)
	}
}

func TestSummaryKeepsExecutionWaitAndDispatchReason(t *testing.T) {
	full := Object{"status": "queued", "dispatchSkip": Object{"reason": "capability", "host": "pc-a"}, "executionWait": "reporter", "executionMissingCapabilities": []any{"race"}, "executionReportError": "投稿の結果を照合中", "executionProgress": Object{"pending": 1, "checks": []any{Object{"criterion": "詳細だけ"}}}}
	summary := summarizeDetail(full)
	if fleet.ObjectOrEmpty(summary["dispatchSkip"])["reason"] != "capability" || summary["executionWait"] != "reporter" || len(fleet.Items(summary["executionMissingCapabilities"])) != 1 || summary["executionReportError"] != full["executionReportError"] {
		t.Fatal(summary)
	}
	if fleet.ObjectOrEmpty(summary["executionProgress"])["pending"] != 1 || fleet.ObjectOrEmpty(summary["executionProgress"])["checks"] != nil {
		t.Fatal(summary)
	}
}
