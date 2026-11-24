package filingcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
)

func TestFilingAlertsProjectionAndChanges(t *testing.T) {
	ctx := context.Background()
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { return allowed(), nil })
	d["status"], d["issue"] = "draft", nil
	d["body"] = strings.Repeat("本文", 100000)
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("会話", 100000)}}
	d["filingCheckCalls"] = Record{"revision": 1, "outcome": "refused", "budget": Record{"evidence": 0, "decisions": 5, "pending": false}, "checks": []any{Record{"at": "2026-11-01", "codeRevision": "old"}}}
	budgetPut(t, s, d)
	fields, err := s.options.Records.ListDraftFields(ctx, consultations.DraftReadFilingAlerts)
	if err != nil || len(fields) != 1 || len(fields[0]) != 7 || fields[0]["body"] != nil || fields[0]["messages"] != nil || obj(fields[0]["filingCheckCalls"])["checks"] == nil {
		t.Fatalf("通知用投影: %v %v", fields, err)
	}
	limit, _ := budgetAlerts(t, s, d)
	if len(limit) != 1 || limit[0]["gist"] != d["title"] || limit[0]["outcome"] != "refused" || limit[0]["lastCheckAt"] != "2026-11-01" {
		t.Fatalf("初回通知: %v", limit)
	}
	budgetAlertCounts(t, limit[0], filingBudget{Decisions: 5})
	// 同一版では json 列を参照できなくても通知を再生成できる。
	if _, err = s.options.Records.DB.ExecContext(ctx, "ALTER TABLE drafts RENAME COLUMN json TO saved_json"); err != nil {
		t.Fatal(err)
	}
	alerts, readErr := s.FilingCheckLimitAlerts(ctx)
	_, restoreErr := s.options.Records.DB.ExecContext(ctx, "ALTER TABLE drafts RENAME COLUMN saved_json TO json")
	if readErr != nil || restoreErr != nil || len(alerts) != 1 {
		t.Fatalf("再読取: %v %v %v", alerts, readErr, restoreErr)
	}
	d["title"] = "更新後"
	budgetPut(t, s, d)
	limit, _ = budgetAlerts(t, s, d)
	if len(limit) != 1 || limit[0]["gist"] != "更新後" {
		t.Fatalf("変更後: %v", limit)
	}
	d["revision"] = 2
	budgetPut(t, s, d)
	if limit, _ = budgetAlerts(t, s, d); len(limit) != 0 {
		t.Fatalf("改訂後: %v", limit)
	}
	// PostgreSQLが解釈できないが既存のデコーダーが受け付ける旧データは全文へ退避する。
	if _, err = s.options.Records.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id=$2`, `{"id":"legacy","status":"draft","body":"legacy"} trailing`, d["id"]); err != nil {
		t.Fatal(err)
	}
	fields, err = s.options.Records.ListDraftFields(ctx, consultations.DraftReadFilingAlerts)
	if err != nil || len(fields) != 1 || fields[0]["body"] != "legacy" {
		t.Fatalf("全文への退避: %v %v", fields, err)
	}
	if _, err = s.options.Records.DB.ExecContext(ctx, `UPDATE drafts SET json='null' WHERE id=$1`, d["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FilingCheckLimitAlerts(ctx); err == nil {
		t.Fatal("不正な相談の読取エラーが通知に伝わりません")
	}
}

func TestEvidenceHoldRecoveryAndExplicitLimit(t *testing.T) {
	ctx := context.Background()
	n := 0
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		n++
		out := allowed()
		out["proceed"] = false
		out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
		return out, nil
	})
	read := func() Record {
		t.Helper()
		r, ok, err := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
		if err != nil || !ok {
			t.Fatal(err)
		}
		return r
	}
	first := s.Check(ctx, d)
	if first == nil || n != 2 {
		t.Fatal(first, n)
	}
	saved := read()
	calls := obj(saved["filingCheckCalls"])
	if fleet.WholeNumber(calls["count"]) != 2 || fleet.Text(obj(calls["evidenceHold"])["reason"]) != first.Error() {
		t.Fatal(calls, first)
	}
	for i := 0; i < 2; i++ {
		err := s.Check(ctx, read())
		if err == nil || err.Error() != first.Error() || n != 2 {
			t.Fatal(err, n)
		}
	}
	if err := s.Check(ctx, read(), "explicit_resend"); err == nil || n != 4 {
		t.Fatal(err, n)
	}
	saved = read()
	calls = obj(saved["filingCheckCalls"])
	if fleet.WholeNumber(calls["count"]) != 4 || obj(fleet.Elements(calls["checks"])[2])["trigger"] != "explicit_resend" {
		t.Fatal(calls)
	}
	(*material)["extra"] = "changed"
	if err := s.Check(ctx, read()); err == nil || n != 6 {
		t.Fatal(err, n)
	}
	for n < 13 {
		before := n
		if err := s.Check(ctx, read(), "explicit_resend"); err == nil || n <= before || n > 13 {
			t.Fatal(err, n)
		}
		if n == 9 {
			saved := read()
			calls := obj(saved["filingCheckCalls"])
			checks := fleet.Elements(calls["checks"])
			if obj(checks[8])["result"] != "refused" {
				t.Fatal(checks[8])
			}
			delete(calls, "budget")
			if _, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return saved, nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Check(ctx, read(), "explicit_resend"); err == nil || n != 13 || !strings.Contains(err.Error(), "読み足しは上限8回") || !strings.Contains(err.Error(), "判定は別枠で上限5回") {
		t.Fatal(err, n)
	}
	if fleet.WholeNumber(obj(read()["filingCheckCalls"])["count"]) != 13 {
		t.Fatal(read())
	}
}

func TestEvidenceHoldLegacyAndClearing(t *testing.T) {
	ctx := context.Background()
	n := 0
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { n++; return allowed(), nil })
	snap := fingerprint(*material)
	d["filingCheckCalls"] = Record{"revision": d["revision"], "count": 2, "checks": []any{Record{"revision": 1, "result": "evidence_requested"}, Record{"revision": 1, "result": "evidence_requested"}}, "evidenceHold": Record{"fingerprint": snap}}
	_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return clone(d), nil })
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "consultation:") || !strings.Contains(err.Error(), "行範囲") || n != 0 {
		t.Fatal(err, n)
	}
	if err = s.Check(ctx, d, "explicit_resend"); err != nil || n != 1 {
		t.Fatal(err, n)
	}
	saved, _, _ := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
	if obj(obj(saved["filingCheckCalls"])["evidenceHold"]) != nil {
		t.Fatal(saved)
	}
	calls := obj(saved["filingCheckCalls"])
	calls["evidenceHold"] = Record{"fingerprint": snap}
	saved["filingCheckCalls"] = calls
	_, err = s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return clone(saved), nil })
	if err != nil {
		t.Fatal(err)
	}
	(*material)["requests"] = []any{}
	obj((*material)["target"])["dependsOnIssues"] = []any{}
	if err = s.Check(ctx, saved); err != nil {
		t.Fatal(err)
	}
	saved, _, _ = s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
	if obj(obj(saved["filingCheckCalls"])["evidenceHold"]) != nil {
		t.Fatal(saved)
	}
}

func TestEvidenceHoldRepairBaseline(t *testing.T) {
	ctx := context.Background()
	n := 0
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { n++; return allowed(), nil })
	d["filingCheckCalls"] = Record{"revision": d["revision"], "count": 5, "outcome": "evidence_requested", "stopKind": "evidence_missing", "evidenceHold": Record{"fingerprint": fingerprint(*material), "reason": "古い理由"}, "recovery": Record{"baseline": Record{"evidence_missing": "0"}, "used": Record{"evidence_missing": []any{"0"}}}}
	_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return clone(d), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(ctx, d); err != nil || n != 1 {
		t.Fatal(err, n)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
	calls := obj(saved["filingCheckCalls"])
	if err != nil || fleet.WholeNumber(calls["count"]) != 1 || obj(calls["evidenceHold"]) != nil || repairBaseline(calls) {
		t.Fatal(saved, err)
	}
}

type holdTestAPI struct{}

func (holdTestAPI) Issues(context.Context, string, string) ([]map[string]any, error) { return nil, nil }
func (holdTestAPI) Issue(context.Context, string, int64) (map[string]any, error)     { return nil, nil }
func (holdTestAPI) CreateIssue(context.Context, string, any) (map[string]any, error) {
	panic("unexpected issue creation")
}

func TestSubmissionTriggersWithRealRecords(t *testing.T) {
	for _, mode := range []string{"evidence", "refused"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			n := 0
			checker, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
				n++
				out := allowed()
				out["proceed"] = false
				out["reason"] = "根拠不足"
				if mode == "evidence" {
					out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
				}
				return out, nil
			})
			d["status"] = "draft"
			d["ready"] = true
			d["questions"] = []any{}
			_, err := checker.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return clone(d), nil })
			if err != nil {
				t.Fatal(err)
			}
			queue, err := jobs.New(checker.options.Records.DB, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			svc, err := filing.New(ctx, filing.Options{Records: checker.options.Records, API: holdTestAPI{}, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Link: func(string, int64) string { return "" }, Check: func(ctx context.Context, d filing.Record) error {
				return checker.Check(ctx, d, filing.CheckTrigger(ctx))
			}, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			principal := auth.Principal{Kind: auth.Screen}
			data := filing.Record{"revision": d["revision"]}
			id := fleet.Text(d["id"])
			if _, err = svc.SubmitAccepted(ctx, id, data, principal); err == nil {
				t.Fatal("expected stop")
			}
			initial := n
			if mode == "evidence" && initial != 2 || mode == "refused" && initial != 1 {
				t.Fatal(n)
			}
			if _, err = svc.Submit(ctx, id, data, principal); err == nil || n != initial {
				t.Fatal(err, n)
			}
			if _, err = svc.SubmitAccepted(ctx, id, data, principal); err == nil || n != initial {
				t.Fatal(err, n)
			}
			limit := 5
			if mode == "evidence" {
				limit = 13
			}
			if _, err = svc.SubmitExplicit(ctx, id, data, principal); err == nil || n <= initial || n > limit {
				t.Fatal(err, n)
			}
			for n < limit {
				before := n
				if _, err = svc.SubmitExplicit(ctx, id, data, principal); err == nil || n <= before || n > limit {
					t.Fatal(err, n)
				}
				after := n
				if _, err = svc.Submit(ctx, id, data, principal); err == nil || n != after {
					t.Fatal(err, n)
				}
			}
			if _, err = svc.SubmitExplicit(ctx, id, data, principal); err == nil || n != limit || !strings.Contains(err.Error(), "上限5回") {
				t.Fatal(err, n)
			}
			saved, _, err := checker.options.Records.Get(ctx, "drafts", id)
			checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
			if err != nil || obj(checks[len(checks)-1])["trigger"] != "explicit_resend" {
				t.Fatal(saved, err)
			}
		})
	}
}

func TestEvidenceHoldPreservesInputBreakdown(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		out["proceed"] = false
		// 3回目と、追加取得なしの最終判定（4回目）は、入力上限で落とされた request:3 を再び要求する。
		number := calls + 2
		if calls >= 3 {
			number = 3
		}
		out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", number), "kind": "body"}}
		return out, nil
	})
	obj((*material)["target"])["body"] = strings.Repeat("t", inputLimit-len(instructions)-len(s.schema)-47000)
	(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("a", 25000)}, Record{"number": 4, "body": strings.Repeat("b", 25000)}}
	first := s.Check(context.Background(), d)
	if first == nil || !strings.Contains(first.Error(), "内訳(bytes/文字)") || calls != 4 {
		t.Fatal(first, calls)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	hold := obj(obj(saved["filingCheckCalls"])["evidenceHold"])
	if !strings.Contains(string(raw(hold["evidenceMissing"])), omissionInputLimitReason) {
		t.Fatal(hold)
	}
	again := s.Check(context.Background(), saved)
	if again == nil || again.Error() != first.Error() || calls != 4 || fleet.Text(hold["reason"]) != first.Error() {
		t.Fatal(first, again, calls)
	}
}

// 予算の検証で使う受付AIの代役。mode で応答を切り替え、呼出しのたびに n を増やす。
func budgetAnswer(mode *string, n *int, during func()) func(context.Context, map[string]any) (map[string]any, error) {
	return func(context.Context, map[string]any) (map[string]any, error) {
		*n++
		if during != nil {
			during()
		}
		out := allowed()
		switch *mode {
		case "ai_failed":
			return nil, errors.New("受付AIの呼び出しに失敗しました")
		case "invalid":
			out["reason"] = " "
		case "refused":
			out["proceed"], out["reason"] = false, "根拠不足"
		case "evidence":
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
		}
		return out, nil
	}
}

func budgetRead(t *testing.T, s *Service, d Record) Record {
	t.Helper()
	r, ok, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	return r
}

func budgetPut(t *testing.T, s *Service, d Record) Record {
	t.Helper()
	if _, err := s.options.Records.Change(context.Background(), "drafts", fleet.Text(d["id"]), func(*sql.Tx, Record) (Record, error) { return clone(d), nil }); err != nil {
		t.Fatal(err)
	}
	return budgetRead(t, s, d)
}

// budgetStore は下書きの起票前判定の記録だけを差し替えて保存し、保存後の記録を返す。
func budgetStore(t *testing.T, s *Service, d, calls Record) Record {
	t.Helper()
	next := clone(d)
	next["status"] = "draft"
	delete(next, "filingCheckHold")
	delete(next, "filingCheckCalls")
	if calls != nil {
		next["filingCheckCalls"] = calls
	}
	return budgetPut(t, s, next)
}

func budgetSaved(d Record) filingBudget {
	b := obj(obj(d["filingCheckCalls"])["budget"])
	return filingBudget{Evidence: int(fleet.WholeNumber(b["evidence"])), Decisions: int(fleet.WholeNumber(b["decisions"])), Pending: b["pending"] == true}
}

func budgetRepairUsed(d Record, cause string) bool {
	for _, v := range fleet.Elements(obj(obj(obj(d["filingCheckCalls"])["recovery"])["used"])[cause]) {
		if v == "1" {
			return true
		}
	}
	return false
}

func budgetRepairRight(cause string) Record {
	return Record{"baseline": Record{cause: "0"}, "used": Record{cause: []any{"0"}}}
}

// budgetAlerts は上限通知と資料不足の集約通知を分けて返す。
func budgetAlerts(t *testing.T, s *Service, d Record) (limit, missing []Record) {
	t.Helper()
	alerts, err := s.FilingCheckLimitAlerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range alerts {
		a := obj(item)
		switch fleet.Text(a["id"]) {
		case "filing-check-limit-" + fleet.Text(d["id"]):
			limit = append(limit, a)
		case "filing-evidence-missing-" + strings.ToLower(fleet.Text(d["project"])):
			missing = append(missing, a)
		default:
			t.Fatalf("想定外の通知: %v", a)
		}
	}
	return limit, missing
}

func budgetAlertCounts(t *testing.T, alert Record, want filingBudget) {
	t.Helper()
	text := fmt.Sprintf("読み足し%d回・判定%d回", want.Evidence, want.Decisions)
	detail := fmt.Sprintf("読み足し%d回/上限8回・判定%d回/上限5回", want.Evidence, want.Decisions)
	if fleet.WholeNumber(alert["evidenceCalls"]) != int64(want.Evidence) || fleet.WholeNumber(alert["decisionCalls"]) != int64(want.Decisions) || fleet.WholeNumber(alert["failures"]) != int64(want.Decisions) || !strings.Contains(fleet.Text(alert["title"]), text) || !strings.Contains(fleet.Text(alert["detail"]), detail) {
		t.Fatalf("通知の回数が予算 %+v と一致しません: %v", want, alert)
	}
}

func TestFilingBudgetAlertsAndRepair(t *testing.T) {
	ctx := context.Background()
	n, mode := 0, "evidence"
	s, d, material := fixture(t, budgetAnswer(&mode, &n, nil))
	base := budgetStore(t, s, d, nil)
	// 製品の経路で読み足しだけを6回まで進める（1回の確認につき新規の要求と同じ要求の繰り返しで2回）。
	if err := s.Check(ctx, base); err == nil || n != 2 {
		t.Fatal(err, n)
	}
	for _, want := range []int{4, 6} {
		if err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend"); err == nil || n != want {
			t.Fatal(err, n)
		}
	}
	saved := budgetRead(t, s, d)
	if got := budgetSaved(saved); got != (filingBudget{Evidence: 6}) {
		t.Fatalf("読み足し6回の保存: %+v", got)
	}
	if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
		t.Fatalf("読み足しだけで上限通知: %v", limit)
	}
	// 修復の基点が有効でも、判定の予算が残る間は初期化しない。
	calls := obj(saved["filingCheckCalls"])
	calls["recovery"] = budgetRepairRight("evidence_missing")
	saved = budgetPut(t, s, saved)
	hold := fleet.Text(obj(obj(saved["filingCheckCalls"])["evidenceHold"])["reason"])
	if err := s.Check(ctx, saved); err == nil || err.Error() != hold || n != 6 {
		t.Fatal(err, n)
	}
	if err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend"); err == nil || n != 8 {
		t.Fatal(err, n)
	}
	for want := 9; want <= 12; want++ {
		if err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend"); err == nil || n != want {
			t.Fatal(err, n)
		}
		saved = budgetRead(t, s, d)
		limit, _ := budgetAlerts(t, s, d)
		if got := budgetSaved(saved); got != (filingBudget{Evidence: 8, Decisions: want - 8}) || len(limit) != 0 || budgetRepairUsed(saved, "evidence_missing") || fleet.WholeNumber(obj(saved["filingCheckCalls"])["count"]) != int64(want) {
			t.Fatalf("判定%d回で通知または初期化: %+v %v %v", want-8, got, limit, saved["filingCheckCalls"])
		}
	}
	// 判定5回で上限通知が出て、回数表示は保存した予算と一致する。
	if err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend"); err == nil || n != 13 {
		t.Fatal(err, n)
	}
	saved = budgetRead(t, s, d)
	limit, _ := budgetAlerts(t, s, d)
	if len(limit) != 1 || limit[0]["outcome"] != obj(saved["filingCheckCalls"])["outcome"] || limit[0]["reason"] != limit[0]["detail"] || limit[0]["draftId"] != d["id"] || limit[0]["requiresUser"] != true || limit[0]["project"] != d["project"] {
		t.Fatalf("判定上限の通知: %v", limit)
	}
	budgetAlertCounts(t, limit[0], budgetSaved(saved))
	budgetAlertCounts(t, limit[0], filingBudget{Evidence: 8, Decisions: 5})
	// 結果待ちの間、終了した相談、別の改訂の記録には上限通知を出さない。
	stopped := clone(saved)
	for name, edit := range map[string]func(Record){
		"予算と記録が結果待ち": func(r Record) {
			c := obj(r["filingCheckCalls"])
			c["pending"] = true
			obj(c["budget"])["pending"] = true
		},
		"予算だけ結果待ち": func(r Record) { obj(obj(r["filingCheckCalls"])["budget"])["pending"] = true },
		"記録だけ結果待ち": func(r Record) { obj(r["filingCheckCalls"])["pending"] = true },
		"起票済み":     func(r Record) { r["status"] = "submitted" },
		"破棄済み":     func(r Record) { r["status"] = "discarded" },
		"別の改訂":     func(r Record) { r["revision"] = 2 },
	} {
		next := clone(stopped)
		edit(next)
		budgetPut(t, s, next)
		if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
			t.Fatalf("%s: 上限通知 %v", name, limit)
		}
	}
	// 保存済みの状態: 読み足し5〜8回・判定5回未満では通知も初期化も起きない。
	snap := fingerprint(*material)
	mode = "refused"
	for evidence := 5; evidence <= 8; evidence++ {
		for _, decisions := range []int{0, 4} {
			name := fmt.Sprintf("読み足し%d回・判定%d回", evidence, decisions)
			state := Record{"revision": 1, "count": evidence + decisions, "checks": []any{}, "outcome": "evidence_requested", "stopKind": "evidence_missing", "budget": Record{"evidence": evidence, "decisions": decisions, "pending": false}, "evidenceHold": Record{"fingerprint": snap, "reason": "資料不足: 保存済みの停止理由"}, "recovery": budgetRepairRight("evidence_missing")}
			saved := budgetStore(t, s, d, state)
			if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
				t.Fatalf("%s: 上限通知 %v", name, limit)
			}
			before := n
			if err := s.Check(ctx, saved); err == nil || err.Error() != "資料不足: 保存済みの停止理由" || n != before {
				t.Fatalf("%s: 自動の再確認 %v 呼出し%d回", name, err, n-before)
			}
			if err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend"); err == nil || n != before+1 {
				t.Fatalf("%s: 明示の再送 %v 呼出し%d回", name, err, n-before)
			}
			saved = budgetRead(t, s, d)
			after := obj(saved["filingCheckCalls"])
			want := filingBudget{Evidence: evidence, Decisions: decisions + 1}
			if got := budgetSaved(saved); got != want || fleet.WholeNumber(after["count"]) != int64(evidence+decisions+1) || after["repairRecheckPending"] != nil || budgetRepairUsed(saved, "evidence_missing") {
				t.Fatalf("%s: 予算が初期化された %+v %v", name, got, after)
			}
			limit, _ := budgetAlerts(t, s, d)
			if decisions == 4 {
				if len(limit) != 1 {
					t.Fatalf("%s: 判定5回の通知 %v", name, limit)
				}
				budgetAlertCounts(t, limit[0], want)
			} else if len(limit) != 0 {
				t.Fatalf("%s: 上限通知 %v", name, limit)
			}
		}
	}
	// 資料不足の集約通知は、同じプロジェクトで3件以上のときに従来どおり出る。
	state := Record{"revision": 1, "count": 5, "checks": []any{}, "outcome": "evidence_requested", "stopKind": "evidence_missing", "budget": Record{"evidence": 5, "decisions": 0, "pending": false}}
	budgetStore(t, s, d, clone(state))
	if _, missing := budgetAlerts(t, s, d); len(missing) != 0 {
		t.Fatalf("資料不足1件で集約通知: %v", missing)
	}
	for _, id := range []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"} {
		other := clone(d)
		other["id"] = id
		budgetStore(t, s, other, clone(state))
	}
	limit, missing := budgetAlerts(t, s, d)
	if len(limit) != 0 || len(missing) != 1 || fleet.WholeNumber(missing[0]["failures"]) != 3 || missing[0]["outcome"] != "evidence_missing" {
		t.Fatalf("資料不足の集約通知: %v %v", limit, missing)
	}
}

func TestFilingBudgetRepairOnce(t *testing.T) {
	ctx := context.Background()
	n, mode := 0, "refused"
	s, d, _ := fixture(t, budgetAnswer(&mode, &n, nil))
	if err := s.Check(ctx, budgetStore(t, s, d, nil)); err == nil || n != 1 {
		t.Fatal(err, n)
	}
	resend := func(want int) error {
		t.Helper()
		err := s.Check(ctx, budgetRead(t, s, d), "explicit_resend")
		if err == nil || n != want {
			t.Fatal(err, n)
		}
		return err
	}
	for want := 2; want <= 5; want++ {
		resend(want)
	}
	if err := resend(5); !strings.Contains(err.Error(), "上限5回") {
		t.Fatal(err)
	}
	// 修復の基点が有効な記録は、判定上限の停止から1回だけ初期化されて再判定できる。
	saved := budgetRead(t, s, d)
	obj(saved["filingCheckCalls"])["recovery"] = budgetRepairRight("refused")
	budgetPut(t, s, saved)
	if limit, _ := budgetAlerts(t, s, d); len(limit) != 1 {
		t.Fatalf("修復前の上限通知: %v", limit)
	}
	resend(6)
	saved = budgetRead(t, s, d)
	calls := obj(saved["filingCheckCalls"])
	if got := budgetSaved(saved); got != (filingBudget{Decisions: 1}) || fleet.WholeNumber(calls["count"]) != 1 || calls["repairRecheckPending"] != true || !budgetRepairUsed(saved, "refused") {
		t.Fatalf("修復後の記録: %+v %v", got, calls)
	}
	if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
		t.Fatalf("修復後の上限通知: %v", limit)
	}
	for want := 7; want <= 10; want++ {
		resend(want)
	}
	// 再読込・再送・自動の再確認では修復権を再使用できない。
	for i := 0; i < 3; i++ {
		resend(10)
	}
	if err := s.Check(ctx, budgetRead(t, s, d)); err == nil || n != 10 {
		t.Fatal(err, n)
	}
	saved = budgetRead(t, s, d)
	if got := budgetSaved(saved); got != (filingBudget{Decisions: 5}) || !budgetRepairUsed(saved, "refused") {
		t.Fatalf("修復の再使用: %+v %v", got, saved["filingCheckCalls"])
	}
	limit, _ := budgetAlerts(t, s, d)
	if len(limit) != 1 {
		t.Fatalf("再停止の上限通知: %v", limit)
	}
	budgetAlertCounts(t, limit[0], filingBudget{Decisions: 5})
	// 結果不明の予約は、修復の基点が有効でも未使用へ戻さない。
	for _, fp := range []any{"別の資料", obj(saved["filingCheckCalls"])["fingerprint"]} {
		unknown := clone(saved)
		calls = obj(unknown["filingCheckCalls"])
		calls["pending"], calls["fingerprint"], calls["recovery"] = true, fp, budgetRepairRight("refused")
		calls["budget"] = Record{"evidence": 0, "decisions": 5, "pending": true}
		budgetPut(t, s, unknown)
		resend(10)
		if err := s.Check(ctx, budgetRead(t, s, d)); err == nil || n != 10 {
			t.Fatal(err, n)
		}
		after := budgetRead(t, s, d)
		limit, _ := budgetAlerts(t, s, d)
		if got := budgetSaved(after); got != (filingBudget{Decisions: 5, Pending: true}) || budgetRepairUsed(after, "refused") || obj(after["filingCheckCalls"])["pending"] != true || len(limit) != 0 {
			t.Fatalf("結果不明の予約が戻った: %+v %v %v", got, after["filingCheckCalls"], limit)
		}
	}
}

func TestFilingBudgetsPersistAcrossResends(t *testing.T) {
	ctx := context.Background()
	t.Run("保存・再読込・再送・失敗の反復", func(t *testing.T) {
		n, mode, reserved := 0, "evidence", 0
		var s *Service
		var d Record
		last := filingBudget{}
		// 受付AIを呼ぶ時点で、予約（結果待ち）が保存済みであることを保存先から読んで確かめる。
		during := func() {
			saved := budgetRead(t, s, d)
			got := budgetSaved(saved)
			if !got.Pending || obj(saved["filingCheckCalls"])["pending"] != true || got.Evidence != last.Evidence || got.Decisions != last.Decisions+1 {
				t.Errorf("呼出し前の予約が保存されていません: %+v（直前 %+v）", got, last)
			}
			reserved++
			// 同じ確認の中で続く呼出しのため、直前の結果を見込みで進める。
			if mode == "evidence" && last.Evidence < filingEvidenceLimit {
				last.Evidence++
			} else {
				last.Decisions++
			}
		}
		s, d, _ = fixture(t, budgetAnswer(&mode, &n, during))
		budgetStore(t, s, d, nil)
		step := func(nextMode, trigger string, wantCalls int, want filingBudget) {
			t.Helper()
			mode = nextMode
			var err error
			if trigger == "" {
				err = s.Check(ctx, budgetRead(t, s, d))
			} else {
				err = s.Check(ctx, budgetRead(t, s, d), trigger)
			}
			saved := budgetRead(t, s, d)
			calls := obj(saved["filingCheckCalls"])
			got := budgetSaved(saved)
			if err == nil || n != wantCalls || got != want || got != last || loadFilingBudget(calls, saved["revision"]) != want || fleet.WholeNumber(calls["count"]) != int64(wantCalls) || got.Evidence+got.Decisions != n || reserved != n {
				t.Fatalf("%s/%s: %v 呼出し%d回 予算%+v 記録%v", nextMode, trigger, err, n, got, calls)
			}
		}
		step("evidence", "", 2, filingBudget{Evidence: 2})
		step("ai_failed", "explicit_resend", 3, filingBudget{Evidence: 2, Decisions: 1})
		step("invalid", "explicit_resend", 4, filingBudget{Evidence: 2, Decisions: 2})
		step("evidence", "", 4, filingBudget{Evidence: 2, Decisions: 2})
		step("evidence", "explicit_resend", 6, filingBudget{Evidence: 4, Decisions: 2})
		step("evidence", "explicit_resend", 8, filingBudget{Evidence: 6, Decisions: 2})
		step("evidence", "explicit_resend", 10, filingBudget{Evidence: 8, Decisions: 2})
		step("ai_failed", "explicit_resend", 11, filingBudget{Evidence: 8, Decisions: 3})
		step("evidence", "explicit_resend", 12, filingBudget{Evidence: 8, Decisions: 4})
		if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
			t.Fatalf("判定4回で上限通知: %v", limit)
		}
		step("invalid", "explicit_resend", 13, filingBudget{Evidence: 8, Decisions: 5})
		// 上限に達した後は、どの応答・契機でも受付AIを呼ばず、予算も戻らない。
		for _, next := range []string{"evidence", "ai_failed", "invalid", "refused", "allowed"} {
			step(next, "explicit_resend", 13, filingBudget{Evidence: 8, Decisions: 5})
			step(next, "", 13, filingBudget{Evidence: 8, Decisions: 5})
		}
		limit, _ := budgetAlerts(t, s, d)
		if len(limit) != 1 {
			t.Fatalf("上限通知: %v", limit)
		}
		budgetAlertCounts(t, limit[0], filingBudget{Evidence: 8, Decisions: 5})
	})
	t.Run("結果待ちの予約", func(t *testing.T) {
		n, mode := 0, "allowed"
		var s *Service
		var d, reservation Record
		var material *Record
		s, d, material = fixture(t, budgetAnswer(&mode, &n, func() { reservation = budgetRead(t, s, d) }))
		if err := s.Check(ctx, budgetStore(t, s, d, nil)); err != nil || n != 1 {
			t.Fatal(err, n)
		}
		// 受付AIの結果を保存する前に止まった状態（予約だけが保存済み）へ戻す。
		budgetPut(t, s, reservation)
		want := filingBudget{Decisions: 1, Pending: true}
		for _, trigger := range []string{"", "explicit_resend", "changed", "explicit_resend"} {
			var err error
			switch trigger {
			case "":
				err = s.Check(ctx, budgetRead(t, s, d))
			case "changed":
				(*material)["extra"] = "changed"
				err = s.Check(ctx, budgetRead(t, s, d))
			default:
				err = s.Check(ctx, budgetRead(t, s, d), trigger)
			}
			saved := budgetRead(t, s, d)
			limit, _ := budgetAlerts(t, s, d)
			if err == nil || n != 1 || budgetSaved(saved) != want || loadFilingBudget(obj(saved["filingCheckCalls"]), saved["revision"]) != want || len(limit) != 0 {
				t.Fatalf("%q: %v 呼出し%d回 %v %v", trigger, err, n, saved["filingCheckCalls"], limit)
			}
		}
	})
}

func TestFilingBudgetLegacyAlertsAndRepair(t *testing.T) {
	ctx := context.Background()
	n, mode := 0, "refused"
	s, d, _ := fixture(t, budgetAnswer(&mode, &n, nil))
	rows := func(results ...string) []any {
		out := []any{}
		for _, result := range results {
			out = append(out, Record{"revision": 1, "result": result, "codeRevision": strings.Repeat("b", 40)})
		}
		return out
	}
	five := []string{"refused", "ai_failed", "invalid_answer", "refused", "allowed"}
	exhausted := filingBudget{Evidence: 8, Decisions: 5}
	// 通知・確認・修復を同じ保存済み記録にかけ、復元した予算どおりに動くことを確かめる。
	run := func(name string, state Record, want filingBudget, alert, called bool) Record {
		t.Helper()
		saved := budgetStore(t, s, d, state)
		if got := loadFilingBudget(obj(saved["filingCheckCalls"]), saved["revision"]); got != want {
			t.Fatalf("%s: 復元した予算 %+v", name, got)
		}
		limit, _ := budgetAlerts(t, s, d)
		if alert != (len(limit) == 1) {
			t.Fatalf("%s: 上限通知 %v", name, limit)
		}
		if alert {
			budgetAlertCounts(t, limit[0], want)
			if limit[0]["codeRevisionChanged"] != (len(fleet.Elements(state["checks"])) > 0) {
				t.Fatalf("%s: コード版の変化 %v", name, limit[0])
			}
		}
		before := n
		err := s.Check(ctx, saved, "explicit_resend")
		if err == nil || called != (n == before+1) || n > before+1 {
			t.Fatalf("%s: %v 呼出し%d回", name, err, n-before)
		}
		return budgetRead(t, s, d)
	}
	// 完全な旧履歴: 読み足しだけ多い記録は止めず、修復権も使わない。
	evidenceHeavy := append(rows("evidence_requested", "evidence_requested", "evidence_requested", "evidence_requested", "evidence_requested"), rows("refused")...)
	after := run("読み足し5回・判定1回", Record{"revision": 1, "count": 6, "checks": evidenceHeavy, "outcome": "refused", "recovery": budgetRepairRight("refused")}, filingBudget{Evidence: 5, Decisions: 1}, false, true)
	if got := budgetSaved(after); got != (filingBudget{Evidence: 5, Decisions: 2}) || fleet.WholeNumber(obj(after["filingCheckCalls"])["count"]) != 7 || budgetRepairUsed(after, "refused") {
		t.Fatalf("読み足しの多い旧履歴が初期化された: %+v %v", got, after["filingCheckCalls"])
	}
	// 完全な旧履歴: 判定5回は上限。コード版が変わっただけでは戻らず、有効な修復の基点があれば1回だけ初期化する。
	run("判定5回", Record{"revision": 1, "count": 5, "checks": rows(five...), "outcome": "refused"}, filingBudget{Decisions: 5}, true, false)
	after = run("判定5回・修復可", Record{"revision": 1, "count": 5, "checks": rows(five...), "outcome": "refused", "recovery": budgetRepairRight("refused")}, filingBudget{Decisions: 5}, true, true)
	if got := budgetSaved(after); got != (filingBudget{Decisions: 1}) || fleet.WholeNumber(obj(after["filingCheckCalls"])["count"]) != 1 || !budgetRepairUsed(after, "refused") {
		t.Fatalf("旧履歴の修復: %+v %v", got, after["filingCheckCalls"])
	}
	// 不完全な旧記録は上限到達として扱い、通知も確認も同じ解釈になる。
	truncated := rows(append(append(append([]string{}, five...), five...), append(append([]string{}, five...), five...)...)...)
	for name, state := range map[string]Record{
		"countだけ":  {"revision": 1, "count": 2, "outcome": "refused"},
		"履歴欠落":     {"revision": 1, "count": 3, "checks": rows("refused"), "outcome": "refused"},
		"切詰め":      {"revision": 1, "count": 21, "checks": truncated, "outcome": "refused"},
		"不正なcount": {"revision": 1, "count": "2", "checks": rows("refused", "refused"), "outcome": "refused"},
		"負のcount":  {"revision": 1, "count": -1, "checks": rows(), "outcome": "refused"},
		"不明な結果":    {"revision": 1, "count": 1, "checks": rows("unknown"), "outcome": "refused"},
		"不正な予算":    {"revision": 1, "count": 1, "checks": rows("refused"), "outcome": "refused", "budget": Record{"evidence": 9, "decisions": 0, "pending": false}},
	} {
		run(name, state, exhausted, true, false)
	}
	after = run("countだけ・修復可", Record{"revision": 1, "count": 2, "outcome": "refused", "recovery": budgetRepairRight("refused")}, exhausted, true, true)
	if got := budgetSaved(after); got != (filingBudget{Decisions: 1}) || !budgetRepairUsed(after, "refused") {
		t.Fatalf("countだけの記録の修復: %+v %v", got, after["filingCheckCalls"])
	}
	// 結果不明の旧記録は通知せず、修復の基点が有効でも初期化しない。
	after = run("結果不明", Record{"revision": 1, "count": 5, "pending": true, "fingerprint": "別の資料", "checks": rows(five...), "outcome": "refused", "recovery": budgetRepairRight("refused")}, exhausted, false, false)
	if calls := obj(after["filingCheckCalls"]); calls["pending"] != true || calls["budget"] != nil || fleet.WholeNumber(calls["count"]) != 5 || budgetRepairUsed(after, "refused") {
		t.Fatalf("結果不明の旧記録が変わった: %v", calls)
	}
	// 相談の改訂が変わった記録は通知の対象外で、新しい改訂の予算で確認する。
	revised := clone(d)
	revised["revision"] = 2
	saved := budgetStore(t, s, revised, Record{"revision": 1, "count": 5, "checks": rows(five...), "outcome": "refused"})
	if limit, _ := budgetAlerts(t, s, d); len(limit) != 0 {
		t.Fatalf("別の改訂の上限通知: %v", limit)
	}
	before := n
	if err := s.Check(ctx, saved, "explicit_resend"); err == nil || n != before+1 {
		t.Fatal(err, n-before)
	}
	after = budgetRead(t, s, d)
	if calls := obj(after["filingCheckCalls"]); budgetSaved(after) != (filingBudget{Decisions: 1}) || fleet.WholeNumber(calls["revision"]) != 2 || fleet.WholeNumber(calls["count"]) != 1 {
		t.Fatalf("新しい改訂の予算: %v", calls)
	}
}
