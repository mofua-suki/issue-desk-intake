package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/deskcleanup"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/notices"
	"sort"
	"strings"
	"time"
)

func (s *Service) setting(ctx context.Context, key string) (any, error) {
	var raw string
	e := s.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM settings WHERE id=?"), key).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var value any
	e = json.Unmarshal([]byte(raw), &value)
	return value, e
}
func (s *Service) table(ctx context.Context, name string) ([]map[string]any, error) {
	var exists int
	if e := s.DB.QueryRowContext(ctx, tableExistsSQL(), name).Scan(&exists); e != nil {
		return nil, e
	}
	if exists == 0 {
		return []map[string]any{}, nil
	}
	var query string
	switch name {
	case "attention_cases":
		query = bindSQL("SELECT json FROM attention_cases ORDER BY " + rowOrder(false))
	case "main_break_cases":
		query = bindSQL("SELECT json FROM main_break_cases ORDER BY " + rowOrder(false))
	default:
		return nil, fmt.Errorf("unknown table %q", name)
	}
	rows, e := s.DB.QueryContext(ctx, query)
	if e != nil {
		return nil, e
	}
	out, _, e := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{})
	return out, e
}

var attentionStates = map[string]string{"pending": "調査の起票待ち（1日の上限）", "rider": "同じ原因の調査に相乗り中", "investigating": "調査中", "retrying": "再試行の送信中", "retried": "再試行済み", "fix": "修正の依頼待ち", "notify": "利用者の判断が必要", "unreadable": "調査の判定を読めません", "ended": "調査が判定なしで終わりました", "retry_error": "再試行を送れませんでした", "file_error": "調査Issueを起票できませんでした", "stopped": "自動の処理を止めました"}

func (s *Service) issueRef(repo string, number any) any {
	var desk any
	if s.Desk != nil {
		desk = s.Desk(repo, int64(fleet.Number(number)))
	}
	var link any
	if s.PublicLink != nil {
		if value := s.PublicLink(repo, "issues", int64(fleet.Number(number))); value != "" {
			link = value
		}
	}
	return map[string]any{"project": repo, "number": number, "desk": desk, "url": link}
}
func (s *Service) savedAttention(ctx context.Context, configured map[string]bool) ([]any, error) {
	rows, e := s.table(ctx, "attention_cases")
	if e != nil {
		return nil, e
	}
	out, waits := s.attentionCaseItems(rows, configured)
	out = append(out, usageWaitItems(waits)...)
	main, e := s.table(ctx, "main_break_cases")
	if e != nil {
		return nil, e
	}
	return append(out, mainBreakItems(main, configured)...), nil
}

// attentionCaseItems は失敗・レビュー保留の記録を要対応の項目にし、利用上限で待っている記録は実行先ごとに分けて返す。
func (s *Service) attentionCaseItems(rows []map[string]any, configured map[string]bool) ([]any, map[string][]map[string]any) {
	out := []any{}
	waits := map[string][]map[string]any{}
	for _, r := range rows {
		if r["kind"] != "failed" && r["kind"] != "review_held" {
			continue
		}
		state := fleet.Text(r["state"])
		wait := fleet.Object(r["usageLimitRetry"])
		_, validAt := wait["at"].(float64)
		if (state == "usage_wait" || state == "retrying") && fleet.Text(wait["provider"]) != "" && validAt {
			provider := fleet.Text(wait["provider"])
			waits[provider] = append(waits[provider], r)
			continue
		}
		if !configured[fleet.Text(r["project"])] || (state == "retried" || state == "fix") && r["left"] == true {
			continue
		}
		label := attentionStates[state]
		if label == "" {
			label = state
		}
		kind := map[string]string{"failed": "失敗", "review_held": "レビュー保留"}[fleet.Text(r["kind"])]
		view := fleet.Object(s.issueRef(fleet.Text(r["project"]), r["number"]))
		for k, v := range map[string]any{"id": "attention:" + fleet.Text(r["key"]), "key": r["key"], "kind": r["kind"], "kindName": kind, "state": state, "stateLabel": label, "title": fleet.Text(r["title"]), "reason": fleet.Text(r["reason"]), "gist": fleet.Text(r["gist"]), "autoCount": fleet.Number(r["autoCount"]), "streak": max(1, fleet.Number(r["streak"])), "verdict": fleet.Object(r["verdict"])["verdict"], "investigation": nil, "draftId": r["draftId"], "sameAs": r["sameAs"], "startedAt": r["startedAt"], "updatedAt": r["updatedAt"]} {
			view[k] = v
		}
		if inv := fleet.Object(r["investigation"]); inv != nil {
			view["investigation"] = s.issueRef(fleet.Text(inv["project"]), inv["number"])
		}
		out = append(out, view)
	}
	return out, waits
}

// usageWaitItems は利用上限で待っている記録を、実行先ごとに件数と再開の見込みを示す1項目にまとめる。
func usageWaitItems(waits map[string][]map[string]any) []any {
	out := []any{}
	providers := []string{}
	for p := range waits {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		rows := waits[p]
		estimate := float64(0)
		for _, r := range rows {
			estimate = max(estimate, fleet.Number(fleet.Object(r["usageLimitRetry"])["at"]))
		}
		at := time.UnixMilli(int64(estimate))
		name := map[string]string{"codex": "Codex", "claude": "Claude", "antigravity": "Antigravity"}[p]
		if name == "" {
			name = p
		}
		out = append(out, map[string]any{"key": "usage-limit-wait:" + p, "kind": "usage_limit_wait", "project": "", "title": fmt.Sprintf("%s: 利用上限で待ち %d件・再開の見込み %s（自動では再試行しません。受付の再試行で進めてください）", name, len(rows), at.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04")), "state": "usage_wait", "stateLabel": "利用上限で待ち", "reason": "", "gist": "", "updatedAt": iso(at)})
	}
	return out
}

// mainBreakItems は main が壊れている記録のうち、まだ解消していないものを要対応の項目にする。
func mainBreakItems(main []map[string]any, configured map[string]bool) []any {
	out := []any{}
	for _, r := range main {
		repo := fleet.Text(r["repo"])
		state := r["state"]
		if !configured[repo] || state == "cleared" || state == "resolved" {
			continue
		}
		suspect, label := "", "main の修正待ち"
		if state == "checking" {
			suspect = "疑い"
			label = "main の確認待ち"
		}
		count := 0
		for _, a := range fleet.Object(r["affected"]) {
			if fleet.Object(a)["done"] != true {
				count++
			}
		}
		reason := fleet.Text(r["reason"])
		if reason == "" {
			reason = "main の確認・修正待ち"
		}
		start := r["startedAt"]
		if start == nil {
			start = r["updatedAt"]
		}
		out = append(out, map[string]any{"key": "main-broken:" + repo, "kind": "main_broken", "project": repo, "title": "main が壊れている" + suspect + "（" + strings.Join(fleet.Strings(r["tests"]), "、") + "）", "state": state, "stateLabel": label, "reason": fmt.Sprintf("%s。待機件数 %d件、待機開始 %v。修正後に自動で再検証します。利用者が先に通す場合は対象Issueの「再試行する」（チャット: issue-desk-issue-control Skillで retest=true）で固定テストを再検証してください。", reason, count, start), "gist": fleet.Object(r["result"])["excerpt"], "updatedAt": r["updatedAt"], "investigation": fleet.Object(r["fix"])["issue"]})
	}
	return out
}
func (s *Service) Attention(ctx context.Context) ([]any, error) {
	return s.AttentionWithTiming(ctx, nil)
}

// AttentionWithTiming reports the duration of each stage without changing its results.
func (s *Service) AttentionWithTiming(ctx context.Context, report func(string, time.Duration)) ([]any, error) {
	measure := func(name string, fn func() ([]any, error)) ([]any, error) {
		start := time.Now()
		rows, err := fn()
		if report != nil {
			report(name, time.Since(start))
		}
		return rows, err
	}
	configured := map[string]bool{}
	if s.Repositories != nil {
		repos, e := s.Repositories()
		if e != nil {
			return nil, e
		}
		for _, repo := range repos {
			configured[repo] = true
		}
	}
	out := []any{}
	if s.DraftAlerts != nil {
		rows, e := measure("drafts", func() ([]any, error) { return s.DraftAlerts(ctx) })
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
	}
	if s.DraftAlerts == nil && report != nil {
		report("drafts", 0)
	}
	rows, e := measure("saved", func() ([]any, error) { return s.savedAttention(ctx, configured) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	ns := notices.Service{DB: s.DB, Now: s.Now, Repositories: s.Repositories}
	rows, e = measure("notices", func() ([]any, error) { return ns.Alerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("stage", func() ([]any, error) { return ns.StageAlerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("incidents", func() ([]any, error) { return s.incidentAlerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("jobs", func() ([]any, error) { return s.jobAttention(ctx, configured) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("jobRecovery", func() ([]any, error) { return s.jobRecoveryAttention(ctx, configured) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("checkout", func() ([]any, error) { return s.checkoutAlerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("rework", func() ([]any, error) { return s.heavyReworkAlerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	rows, e = measure("rc", func() ([]any, error) { return s.rcAlerts(ctx) })
	if e != nil {
		return nil, e
	}
	out = append(out, rows...)
	return measure("visible", func() ([]any, error) {
		if s.IncludeArchived {
			return out, nil
		}
		return deskcleanup.VisibleAlerts(ctx, s.DB, "attention", out)
	})
}
func (s *Service) incidentAlerts(ctx context.Context) ([]any, error) {
	saved, e := s.OperationalSetting(ctx, "incident-deadline")
	if e != nil {
		return nil, e
	}
	out := []any{}
	labels := map[string]string{"waiting": "対応待ち", "user": "利用者が対応", "resolved": "解消", "expired": "期限切れ", "acting": "対処中", "acted": "対処済み"}
	kinds := map[string]string{"stall": "止まり", "monitor_stopped": "ホームの監視の停止", "full_test": "main の全部のテストの失敗", "main_broken": "main の共通の失敗", "stage_watch": "段取り運用の異常結果", "slow": "作業の遅延"}
	for _, raw := range fleet.List(saved) {
		r := fleet.Object(raw)
		state := fleet.Text(r["state"])
		ended := r["endedAt"] != nil || state == "resolved"
		if r["acknowledgedAt"] != nil || ended && (state == "resolved" || state == "acted") && !slowFilingNeedsAttention(r) {
			continue
		}
		view := map[string]any{}
		for k, v := range r {
			view[k] = v
		}
		updated := any(nil)
		if history := fleet.List(r["history"]); len(history) > 0 {
			updated = fleet.Object(history[len(history)-1])["at"]
		}
		target := fleet.Object(r["target"])
		view["key"], view["sourceKey"], view["kind"], view["kindName"], view["stateLabel"], view["project"], view["number"], view["updatedAt"], view["reason"] = "incident:"+fleet.Text(r["id"]), r["key"], "incident", kinds[fleet.Text(r["kind"])], labels[state], target["project"], target["number"], updated, kinds[fleet.Text(r["kind"])]
		if slowFilingNeedsAttention(r) {
			filing := fleet.Object(r["slowFiling"])
			message := fleet.Text(filing["message"])
			if message == "" {
				message = "起票送信の成否を確認できません。再送しません。"
				if filing["directPR"] == true {
					message += " 直接PRが必要です。"
				}
			}
			view["reason"] = message
		}
		out = append(out, view)
	}
	return out, nil
}
