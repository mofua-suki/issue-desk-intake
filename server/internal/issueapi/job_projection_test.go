package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"testing"
	"time"
)

func TestJobProjectionValidatesMachineFieldsAndRetainsActiveClosedState(t *testing.T) {
	hold := "deletions"
	job := &jobs.Job{Status: "held", HoldReason: &hold, View: map[string]any{"workType": "inherited", "review": Object{"kind": "ai", "head": strings.Repeat("a", 40), "provider": "codex", "model": "model", "basis": "default", "arbitrary": "not projected"}, "deletionApproval": Object{"head": strings.Repeat("b", 40), "items": []any{Object{"kind": "delete", "path": "src/a.go", "arbitrary": "not projected"}}}}, Result: Object{"error": strings.Repeat("x", 3000)}}
	s := &Service{}
	view, e := s.jobProjection(context.Background(), "owner/repo", "review_held", job)
	if e != nil || view["workType"] != "unknown" || len(fleet.Text(view["error"])) != 2000 || fleet.ObjectOrEmpty(view["review"])["arbitrary"] != nil || fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(view["deletionApproval"])["items"])[0])["arbitrary"] != nil {
		t.Fatalf("projection %v %v", view, e)
	}
	job.View["deletionApproval"] = Object{"head": strings.Repeat("b", 40), "items": []any{Object{"kind": "rename", "path": "src/a.go"}}}
	view, _ = s.jobProjection(context.Background(), "owner/repo", "review_held", job)
	if view["deletionApproval"] != nil {
		t.Fatal("malformed rename accepted")
	}
	// A data-deletion SQL hold lists every file the PR changes as plain paths.
	job.View["deletionApproval"] = Object{"head": strings.Repeat("b", 40), "items": []any{"src/store.py", Object{"kind": "delete", "path": "old.py"}}, "total": 2}
	view, _ = s.jobProjection(context.Background(), "owner/repo", "review_held", job)
	d := fleet.ObjectOrEmpty(view["deletionApproval"])
	mixed := fleet.Items(d["items"])
	if len(mixed) != 2 || fleet.ObjectOrEmpty(mixed[0])["kind"] != "changed" || fleet.ObjectOrEmpty(mixed[0])["path"] != "src/store.py" || fleet.ObjectOrEmpty(mixed[1])["kind"] != "delete" || d["head"] != strings.Repeat("b", 40) || d["reason"] != "deletions" || d["scope"] != "data_deletion_sql" {
		t.Fatalf("data-deletion SQL changed files not projected: %v", view["deletionApproval"])
	}
	for _, bad := range []any{"", 7, Object{"kind": "sql", "path": "x"}} {
		job.View["deletionApproval"] = Object{"head": strings.Repeat("b", 40), "items": []any{"src/store.py", bad}}
		view, _ = s.jobProjection(context.Background(), "owner/repo", "review_held", job)
		if view["deletionApproval"] != nil {
			t.Fatalf("invalid item %v accepted", bad)
		}
	}
	many := []any{}
	for i := 0; i < 130; i++ {
		many = append(many, fmt.Sprintf("src/f%03d.py", i))
	}
	job.View["deletionApproval"] = Object{"head": strings.Repeat("b", 40), "items": many, "total": 130}
	view, _ = s.jobProjection(context.Background(), "owner/repo", "review_held", job)
	d = fleet.ObjectOrEmpty(view["deletionApproval"])
	if len(fleet.Items(d["items"])) != 100 || d["total"] != 130 {
		t.Fatalf("long list not clipped with total: %d %v", len(fleet.Items(d["items"])), d["total"])
	}
	body := deletionApprovalAudit(Object{"account": "a", "at": "t", "items": []any{"src/store.py", "evil\n<!-- x"}}, "id", strings.Repeat("b", 40), "token", "nas", "発言", "2026-11-23T08:03:32+09:00")
	if !strings.Contains(body, "データ削除SQLを含むPRの承認の記録（チャットの利用者の承認）") || !strings.Contains(body, "- 変更ファイル: src/store.py") || !strings.Contains(body, "- 変更ファイル: evil &lt;!-- x") || strings.Count(body, "<!--") != 1 {
		t.Fatalf("audit body %q", body)
	}
	if body := deletionApprovalAudit(Object{"account": "a", "at": "t", "items": []any{Object{"kind": "delete", "path": "old"}}}, "id", strings.Repeat("b", 40), "screen", "", "", ""); !strings.HasPrefix(body, "削除・改名・種別変更の承認の記録（受付画面での操作）") || !strings.Contains(body, "- delete: old") {
		t.Fatalf("file deletion audit changed %q", body)
	}
	if status := shownStatus(Object{"state": "closed"}, &jobs.Job{Status: "running", View: Object{}}, false); status != "running" {
		t.Fatal("in-flight closed issue hidden")
	}
	if status := shownStatus(Object{"state": "closed", "labels": []any{"state/completed"}}, nil, false); status != "completed" {
		t.Fatal("legacy completed label discarded")
	}
	if status := shownStatus(Object{"state": "closed", "labels": []any{"state/completed", "state/failed"}}, nil, false); status != "closed" {
		t.Fatal("ambiguous legacy state adopted")
	}
}

func TestReviewFindingsProjectionReturnsReviewText(t *testing.T) {
	findings := []any{"軽微な指摘の本文", strings.Repeat("長", reviewFindingsMaxLength+10)}
	review := reviewFindingsProjection(Object{"status": "held", "summary": "要約", "review": map[string]any{"decision": "approve", "findings": findings, "findingKinds": []any{"regression", "other"}, "findingSeverities": []any{"minor", "minor"}, "uncertainties": []any{"不確実な点"}, "arbitrary": "not projected"}})
	list := fleet.Items(review["findings"])
	if review["previous"] != false || review["decision"] != "approve" || len(list) != 2 || list[0] != "軽微な指摘の本文" || len([]rune(fleet.Text(list[1]))) != reviewFindingsMaxLength || fleet.Items(review["uncertainties"])[0] != "不確実な点" || review["arbitrary"] != nil {
		t.Fatalf("review findings %v", review)
	}
	// 実装AIの質問で止まりレビューの無い回も、前回の指摘を読める。
	review = reviewFindingsProjection(Object{"status": "held", "previousReview": map[string]any{"decision": "hold", "breaking": true, "findings": []any{"前回の指摘"}}})
	if review["previous"] != true || review["breaking"] != true || fleet.Items(review["findings"])[0] != "前回の指摘" {
		t.Fatalf("previous review %v", review)
	}
	// 今回のレビューがあるときは前回を返さない。
	review = reviewFindingsProjection(Object{"status": "held", "review": map[string]any{"decision": "approve"}, "previousReview": map[string]any{"decision": "hold"}})
	if review["decision"] != "approve" {
		t.Fatalf("current review must win %v", review)
	}
	for _, value := range []Object{{"status": "held"}, {"status": "held", "review": map[string]any{"omitted": true, "reason": "上限"}}, {"status": "held", "review": "broken"}} {
		if reviewFindingsProjection(value) != nil {
			t.Fatalf("no findings expected %v", value)
		}
	}
}

// 新しいWorkerの結果は理由を userJudgments・resolvableUncertainties に書く。
// これらが読めないと decision: hold の理由が分からなくなる。旧形式の uncertainties も読める。
func TestReviewFindingsProjectionReturnsClassifiedUncertainties(t *testing.T) {
	review := reviewFindingsProjection(Object{"review": map[string]any{"decision": "hold", "findings": []any{}, "userJudgments": []any{"利用者の選択"}, "resolvableUncertainties": []any{"追加確認"}, "postMergeChecks": []any{"配置後に確認"}}})
	if fleet.Items(review["userJudgments"])[0] != "利用者の選択" || fleet.Items(review["resolvableUncertainties"])[0] != "追加確認" || fleet.Items(review["postMergeChecks"])[0] != "配置後に確認" || len(fleet.Items(review["uncertainties"])) != 0 {
		t.Fatalf("classified %v", review)
	}
	review = reviewFindingsProjection(Object{"review": map[string]any{"decision": "hold", "uncertainties": []any{"過去の結果"}}})
	if fleet.Items(review["uncertainties"])[0] != "過去の結果" || review["userJudgments"] == nil || len(fleet.Items(review["userJudgments"])) != 0 {
		t.Fatalf("legacy %v", review)
	}
}

func TestReviewFindingsProjectionRedactsEveryReturnedString(t *testing.T) {
	const leak = "token=abc123 と sk-LEAKED456"
	raw := map[string]any{"decision": "token=abc123"}
	keys := []string{"findings", "findingKinds", "findingSeverities", "uncertainties", "userJudgments", "resolvableUncertainties", "followups", "improvements", "postMergeChecks"}
	for _, key := range keys {
		raw[key] = []any{leak}
	}
	for _, source := range []string{"review", "previousReview"} {
		review := reviewFindingsProjection(Object{source: raw})
		if text := fmt.Sprint(review); strings.Contains(text, "abc123") || strings.Contains(text, "LEAKED456") {
			t.Fatalf("%s の資格情報が残っています: %s", source, text)
		}
		for _, key := range keys {
			if !strings.Contains(fleet.Text(fleet.Items(review[key])[0]), "[REDACTED]") {
				t.Fatalf("%s %s %v", source, key, review[key])
			}
		}
	}
	// 長さの上限は伏せた後の本文にも掛かる。
	long := reviewFindingsProjection(Object{"review": map[string]any{"findings": []any{"token=abc " + strings.Repeat("あ", reviewFindingsMaxLength)}}})
	if text := fleet.Text(fleet.Items(long["findings"])[0]); len([]rune(text)) != reviewFindingsMaxLength || !strings.HasPrefix(text, "[REDACTED]") {
		t.Fatal(text)
	}
}

// 前回のレビューが tooLarge や breaking で findings が空でも、保留の理由を読める。
func TestReviewFindingsProjectionReturnsRedactedSummary(t *testing.T) {
	review := reviewFindingsProjection(Object{"previousReview": map[string]any{"decision": "hold", "tooLarge": true, "summary": "差分が大きい token=abc123", "findings": []any{}}})
	if review["previous"] != true || review["tooLarge"] != true || !strings.HasPrefix(fleet.Text(review["summary"]), "差分が大きい") || strings.Contains(fleet.Text(review["summary"]), "abc123") {
		t.Fatalf("summary %v", review)
	}
	long := reviewFindingsProjection(Object{"review": map[string]any{"summary": strings.Repeat("長", reviewFindingsMaxLength+10)}})
	if len([]rune(fleet.Text(long["summary"]))) != reviewFindingsMaxLength {
		t.Fatal("summary の長さ")
	}
}

// error は一覧と詳細の両方に載るので、同じ結果の summary と同じように伏せる。
func TestJobProjectionRedactsError(t *testing.T) {
	s := &Service{}
	for _, key := range []string{"error", "reason"} {
		for _, status := range []string{"failed", "stopped", "review_held", "closed"} {
			view, e := s.jobProjection(context.Background(), "owner/repo", status, &jobs.Job{View: map[string]any{}, Result: Object{key: "失敗 token=abc123 と sk-LEAKED456"}})
			if e != nil {
				t.Fatal(e)
			}
			text := fleet.Text(view["error"])
			if !strings.HasPrefix(text, "失敗") || strings.Contains(text, "abc123") || strings.Contains(text, "LEAKED456") {
				t.Fatalf("%s %s: %q", key, status, text)
			}
		}
	}
}

func TestResultProjectionRedactsNestedStringsAndOmitsReviewText(t *testing.T) {
	const leak = "password=hunter2 sk-LEAKED456"
	got := resultProjection(Object{"status": "failed", "summary": leak, "questions": []any{leak}, "unverified": []any{leak}, "incomplete": map[string]any{"reason": leak, "detail": []any{map[string]any{"text": leak}}, "count": float64(2)}, "prerequisiteMissing": []any{leak}, "fixedTestCause": map[string]any{"acquisition": leak}, "review": map[string]any{"decision": "hold", "findings": []any{"本文"}}})
	if text := fmt.Sprint(got); strings.Contains(text, "hunter2") || strings.Contains(text, "LEAKED456") {
		t.Fatalf("結果の資格情報が残っています: %s", text)
	}
	if fleet.ObjectOrEmpty(got["incomplete"])["count"] != float64(2) {
		t.Fatalf("数値を変えました %v", got)
	}
	// 一覧・検索・画面の同期が共通で使う結果には本文を載せない。
	if _, has := got["reviewFindings"]; has {
		t.Fatalf("一覧の結果にレビュー本文があります %v", got)
	}
}

// 本文は詳細だけで返す。一覧・1行の更新の行には載らず、詳細では新しい欄と伏せた文字列を読める。
func TestReviewFindingsAreOnlyInDetailPostgres(t *testing.T) {
	f := newFixture(t)
	// Keep the run limit out of this test of current versus historical judgments.
	f.options.RunLimit = 10
	f.restart()
	raw, e := json.Marshal(Object{"status": "held", "summary": "要約 token=abc123", "review": Object{"decision": "hold", "findings": []any{}, "userJudgments": []any{"利用者の選択 sk-LEAKED456"}, "resolvableUncertainties": []any{"追加確認 token=abc123"}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='review_held',result=$2::jsonb WHERE id=$1`, f.job, string(raw)); e != nil {
		t.Fatal(e)
	}
	detail, e := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if e != nil {
		t.Fatal(e)
	}
	review := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(detail["result"])["reviewFindings"])
	if !strings.HasPrefix(fleet.Text(fleet.Items(review["userJudgments"])[0]), "利用者の選択") || !strings.HasPrefix(fleet.Text(fleet.Items(review["resolvableUncertainties"])[0]), "追加確認") {
		t.Fatalf("詳細 %v", detail["result"])
	}
	if text := fmt.Sprint(detail["result"]); strings.Contains(text, "abc123") || strings.Contains(text, "LEAKED456") {
		t.Fatalf("詳細の資格情報 %s", text)
	}
	rows, e := f.s.List(f.ctx)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	entry, e := f.s.ListEntry(f.ctx, "owner/repo", 1)
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range []Object{rows[0], entry} {
		if row["requiresUser"] != true {
			t.Fatalf("current user judgment hidden: %v", row)
		}
		result := fleet.ObjectOrEmpty(row["result"])
		if result == nil || result["reviewFindings"] != nil {
			t.Fatalf("一覧の行 %v", result)
		}
		if text := fmt.Sprint(result); strings.Contains(text, "abc123") || strings.Contains(text, "利用者の選択") {
			t.Fatalf("一覧の行の本文・資格情報 %s", text)
		}
	}
	if _, e = f.pg.Exec(`UPDATE jobs SET result=jsonb_build_object('status','held','previousReview',result->'review') WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	entry, e = f.s.ListEntry(f.ctx, "owner/repo", 1)
	if e != nil || entry["requiresUser"] == true {
		t.Fatalf("old judgment reused: %v %v", entry, e)
	}
}

// 本文の伏せる処理が、終了メモの目印を消さない。
func TestCloseNoteBodyKeepsMarkerAfterRedaction(t *testing.T) {
	r := Object{"id": "close-1", "reason": "completed", "comment": "-----BEGIN PRIVATE KEY-----\nabc123", "description": "Cookie: session=def456"}
	body := closeNoteBody(r)
	if !strings.HasSuffix(body, closeNoteMarker(r)) || strings.Contains(body, "abc123") || strings.Contains(body, "def456") {
		t.Fatal(body)
	}
}

func TestSweepFailureAppearsInListAndDetailPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`UPDATE jobs SET status='running', lease_host='pc-last', lease_expires_at='2026-10-31T23:59:00Z', view='{"phase":"publishing","lastExecutionError":{"kind":"api_rejected","at":"2026-10-31T23:59:00Z","stage":"publishing","reason":"token=abc123","intakeHttpStatus":"409","intakeCode":"turn_lost","giteaHttpStatus":"503","unknown":"secret-value"}}', result='{"keep":42}' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,at,data) VALUES ($1,'lease_expired',now(),'{"generation":0}'),($1,'lease_expired',now(),'{"generation":0}')`, f.job); err != nil {
		t.Fatal(err)
	}
	if err := f.s.o.Jobs.Sweep(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := f.s.o.Jobs.Detail(f.ctx, f.job)
	if err != nil {
		t.Fatal(err)
	}
	job := detail["job"].(*jobs.Job)
	want := "lease の期限切れが同じ世代で3回続いたため失敗にしました。最後のPC: pc-last"
	if job.Result["summary"] != want || job.Result["lastHost"] != "pc-last" || job.Result["lastStage"] != "publishing" || job.Result["leaseExpiredCount"] != float64(3) || job.Result["lastExecutionError"] == nil || job.Result["keep"] != float64(42) {
		t.Fatalf("detail: %v", job.Result)
	}
	apiDetail, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	projected := fleet.ObjectOrEmpty(apiDetail["result"])
	execution := fleet.ObjectOrEmpty(projected["lastExecutionError"])
	if projected["leaseExpiredCount"] != float64(3) || projected["lastHost"] != "pc-last" || projected["lastStage"] != "publishing" || execution["kind"] != "api_rejected" || execution["intakeHttpStatus"] != "409" || execution["intakeCode"] != "turn_lost" || execution["giteaHttpStatus"] != "503" || execution["stage"] != "publishing" || execution["at"] == nil || execution["reason"] == nil || execution["unknown"] != nil || strings.Contains(fmt.Sprint(execution), "abc123") {
		t.Fatalf("detail diagnostics: %v", projected)
	}
	events := fleet.Items(fleet.ObjectOrEmpty(apiDetail["dispatchRecord"])["events"])
	if len(events) < 1 || fleet.ObjectOrEmpty(events[0])["kind"] != "lease_expired" || fleet.ObjectOrEmpty(events[0])["at"] == nil || fleet.ObjectOrEmpty(events[0])["generation"] != float64(0) || fleet.ObjectOrEmpty(events[0])["to"] != "failed" {
		t.Fatalf("events: %v", events)
	}
	summary, err := f.s.Detail(f.ctx, "owner/repo", 1, false, true)
	if err != nil {
		t.Fatal(err)
	}
	summaryEvents, _ := json.Marshal(fleet.ObjectOrEmpty(summary["dispatchRecord"])["events"])
	detailEvents, _ := json.Marshal(events)
	if string(summaryEvents) != string(detailEvents) {
		t.Fatalf("summary history differs from detail: %s / %s", summaryEvents, detailEvents)
	}
	rows, err := f.s.List(f.ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	result := fleet.ObjectOrEmpty(rows[0]["result"])
	for _, key := range []string{"leaseExpiredCount", "lastHost", "lastStage", "lastExecutionError"} {
		if _, ok := result[key]; ok {
			t.Fatalf("list exposes %s: %v", key, result)
		}
	}
	for _, event := range fleet.Items(fleet.ObjectOrEmpty(rows[0]["dispatchRecord"])["events"]) {
		if fleet.ObjectOrEmpty(event)["kind"] == "lease_expired" {
			t.Fatalf("list event: %v", event)
		}
	}
	if result["status"] != "failed" || result["summary"] != want {
		t.Fatalf("list result: %v", result)
	}
}

func TestLeaseDiagnosticsProjectionLegacyAndRedaction(t *testing.T) {
	if len(leaseDiagnosticsProjection(Object{"status": "failed"})) != 0 {
		t.Fatal("legacy result changed")
	}
	got := leaseDiagnosticsProjection(Object{"leaseExpiredCount": "3", "lastHost": "token=abc123 " + strings.Repeat("x", 200), "lastStage": 7, "lastExecutionError": map[string]any{"kind": "timeout", "reason": "sk-LEAKED456 " + strings.Repeat("x", 200), "other": "hidden", "stage": 3}})
	if got["leaseExpiredCount"] != nil || got["lastStage"] != nil || len([]rune(fleet.Text(got["lastHost"]))) > 100 || strings.Contains(fmt.Sprint(got), "abc123") || strings.Contains(fmt.Sprint(got), "LEAKED456") || fleet.ObjectOrEmpty(got["lastExecutionError"])["other"] != nil || fleet.ObjectOrEmpty(got["lastExecutionError"])["stage"] != nil || len([]rune(fleet.Text(fleet.ObjectOrEmpty(got["lastExecutionError"])["reason"]))) > 100 {
		t.Fatalf("diagnostics: %v", got)
	}
	if len(leaseDiagnosticsProjection(Object{"lastExecutionError": "bad"})) != 0 {
		t.Fatal("non-object accepted")
	}
	if len(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"unknown": "ignored", "stage": 3}})) != 0 {
		t.Fatal("empty sanitized execution error was returned")
	}
}

func TestLeaseExpiredEventsRespectDetailLimitPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,at,data) SELECT $1,'lease_expired', '2026-01-01T00:00:00Z'::timestamptz + n * interval '1 second', jsonb_build_object('generation',n,'to','queued') FROM generate_series(1,25) AS n ORDER BY n`, f.job); err != nil {
		t.Fatal(err)
	}
	detail, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	events := fleet.Items(fleet.ObjectOrEmpty(detail["dispatchRecord"])["events"])
	if len(events) != 20 {
		t.Fatalf("event count: %d", len(events))
	}
	for i, event := range events {
		if fleet.ObjectOrEmpty(event)["generation"] != float64(25-i) || fleet.ObjectOrEmpty(event)["to"] != "queued" || fleet.ObjectOrEmpty(event)["at"] == nil {
			t.Fatalf("event %d: %v", i, event)
		}
	}
}

func TestDispatchSkipMixedHistoryProjectionPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES ($1,'dispatch_skip','old','2026-11-01T00:05:00Z','{"reason":"scope_reserved","by":2}'),($1,'dispatch_skip','mid','2026-11-01T00:04:00Z','{"reason":"overlap","by":3}'),($1,'dispatch_skip','new','2026-11-01T00:01:00Z','{"reason":"future_reason","preferredHost":"private-value","slots":0,"repoLimit":4}')`, f.job); err != nil {
		t.Fatal(err)
	}
	detail, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.s.List(f.ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	for _, v := range []Object{detail, list[0]} {
		skip := fleet.ObjectOrEmpty(v["dispatchSkip"])
		if skip["reason"] != "future_reason" || skip["host"] != "new" {
			t.Fatalf("skip %v", skip)
		}
		events := fleet.Items(fleet.ObjectOrEmpty(v["dispatchRecord"])["events"])
		if len(events) < 3 || fleet.ObjectOrEmpty(events[0])["reason"] != "future_reason" || fleet.ObjectOrEmpty(events[1])["reason"] != "overlap" || fleet.ObjectOrEmpty(events[2])["reason"] != "scope_reserved" {
			t.Fatalf("events %v", events)
		}
		if strings.Contains(fmt.Sprint(v), "private-value") || fleet.ObjectOrEmpty(events[0])["slots"] != nil || fleet.ObjectOrEmpty(events[0])["repoLimit"] != nil || fleet.ObjectOrEmpty(events[0])["preferredHost"] != nil {
			t.Fatalf("unexpected fields %v", v)
		}
	}
}

// 断った最終報告の理由は詳細の配布記録に伏せ字・長さ制限付きで出し、一覧には出さない。
func TestReportRejectionAppearsInDetailOnlyPostgres(t *testing.T) {
	f := newFixture(t)
	data := `{"code":"","status":400,"reportKind":"merged","count":2,"firstAt":"2026-11-20T04:22:14Z","lastAt":"2026-11-20T04:50:15Z","epoch":2,"message":"result は32768文字以内のJSONオブジェクトにしてください。token=abc123 ` + strings.Repeat("x", 400) + `","diagnostic":{"generation":0}}`
	// 断りは最初に記録され（seqが最も古い）、再発で時刻だけ新しくなる。その後に lease の出来事が20件を超えて積まれても消えない。
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,at,host,data) VALUES ($1,'report_rejected','2026-11-20T04:50:15Z','pc-last',$2::jsonb)`, f.job, data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,at,data) SELECT $1,'lease_expired','2026-11-20T04:00:00Z'::timestamptz + n * interval '1 second',jsonb_build_object('generation',0,'to','queued') FROM generate_series(1,25) AS n ORDER BY n`, f.job); err != nil {
		t.Fatal(err)
	}
	apiDetail, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var rejection Object
	listed := fleet.Items(fleet.ObjectOrEmpty(apiDetail["dispatchRecord"])["events"])
	if len(listed) != 21 || fleet.ObjectOrEmpty(listed[0])["kind"] != "report_rejected" || fleet.ObjectOrEmpty(listed[0])["sortAt"] != nil {
		t.Fatalf("events: %v", listed)
	}
	for _, event := range listed {
		if fleet.ObjectOrEmpty(event)["kind"] == "report_rejected" {
			rejection = fleet.ObjectOrEmpty(event)
		}
	}
	message, _ := rejection["message"].(string)
	if rejection["status"] != float64(400) || rejection["reportKind"] != "merged" || rejection["count"] != float64(2) || rejection["lastAt"] != "2026-11-20T04:50:15Z" || rejection["host"] != "pc-last" ||
		!strings.HasPrefix(message, "result は32768文字以内") || strings.Contains(message, "abc123") || len([]rune(message)) > 300 || rejection["diagnostic"] != nil || rejection["epoch"] != nil {
		t.Fatalf("rejection: %v", rejection)
	}
	rows, err := f.s.List(f.ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	for _, event := range fleet.Items(fleet.ObjectOrEmpty(rows[0]["dispatchRecord"])["events"]) {
		if fleet.ObjectOrEmpty(event)["kind"] == "report_rejected" {
			t.Fatalf("list event: %v", event)
		}
	}
}

// 断った報告は更新後の時刻の位置に入り、ほかの出来事の新しい順を崩さない。
func TestWithRejectionsOrdersByUpdatedTime(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 20, 4, m, 0, 0, time.UTC) }
	events := []any{Object{"kind": "leased", "sortAt": at(30)}, Object{"kind": "lease_expired", "sortAt": at(20)}, Object{"kind": "leased", "sortAt": at(10)}}
	rejections := []Object{{"kind": "report_rejected", "code": "a", "sortAt": at(15)}, {"kind": "report_rejected", "code": "b", "sortAt": at(40)}}
	got := withRejections(events, rejections)
	want := []string{"report_rejected", "leased", "lease_expired", "report_rejected", "leased"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i, raw := range got {
		event := raw.(Object)
		if event["kind"] != want[i] || event["sortAt"] != nil {
			t.Fatalf("%d: %v", i, got)
		}
	}
	if got[0].(Object)["code"] != "b" || got[3].(Object)["code"] != "a" {
		t.Fatal(got)
	}
}
