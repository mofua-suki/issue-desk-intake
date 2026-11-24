package startup

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/references"
	"regexp"
	"strings"
)

var projectIssueReference = regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]{0,8})(?:[^0-9A-Za-z_]|$)`)

func referenceEntries(d consultations.Record) []map[string]any {
	var latest map[string]any
	for _, raw := range fleet.List(d["messages"]) {
		m := fleet.Object(raw)
		if m["role"] == "user" {
			latest = m
		}
	}
	entries := append([]map[string]any{}, objects(latest["references"])...)
	source := fleet.Text(latest["text"])
	for _, n := range deskReferences(source) {
		entries = append(entries, map[string]any{"desk": n})
	}
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		for _, m := range projectIssueReference.FindAllStringSubmatch(line, -1) {
			var n int64
			for _, r := range m[2] {
				n = n*10 + int64(r-'0')
			}
			entries = append(entries, map[string]any{"project": m[1], "gitea": n})
		}
	}
	return entries
}
func objects(v any) []map[string]any {
	out := []map[string]any{}
	for _, raw := range fleet.List(v) {
		if r := fleet.Object(raw); r != nil {
			out = append(out, r)
		}
	}
	return out
}
func (a *App) referenceDesks(ctx context.Context, d consultations.Record) ([]int64, error) {
	project := fleet.Text(d["project"])
	entries := referenceEntries(d)
	if len(entries) == 0 {
		return []int64{}, nil
	}
	missing := func() (bool, error) {
		for _, r := range entries {
			if n, valid := positiveNumber(r["desk"]); valid {
				_, ok, e := a.Numbers.Issue(ctx, n)
				if e != nil {
					return false, e
				}
				if !ok {
					return true, nil
				}
			} else if project != "" && strings.EqualFold(project, fleet.Text(r["project"])) {
				if n, valid := positiveNumber(r["gitea"]); valid {
					_, ok, e := a.Numbers.Number(ctx, project, n)
					if e != nil {
						return false, e
					}
					if !ok {
						return true, nil
					}
				}
			}
		}
		return false, nil
	}
	if e := a.refreshMissingNumbers(ctx, missing); e != nil {
		return nil, e
	}
	return a.collectDeskNumbers(ctx, entries, project)
}

// refreshMissingNumbers は対応の欠けた参照があれば番号表を取り直す。一時的な失敗は返し、それ以外は記録して確認できる対応だけで続ける。
func (a *App) refreshMissingNumbers(ctx context.Context, missing func() (bool, error)) error {
	needed, e := missing()
	if e != nil {
		return e
	}
	if needed {
		e = a.numbering(ctx, false)
		if e == nil {
			if needed, e = missing(); e == nil && needed {
				e = a.numbering(ctx, true)
			}
		}
		if e != nil {
			var remote *gitea.Error
			temporary := errors.As(e, &remote) && (remote.Status == 0 || remote.Status == 408 || remote.Status == 429 || remote.Status >= 500)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if temporary {
				return e
			}
			if a.Log != nil {
				a.Log("共通番号の対応表を取得できませんでした。既に確認できる対応だけを使います。原因: " + contract.SafeCause(e))
			}
		}
	}
	return nil
}

// collectDeskNumbers は参照を共通番号に直し、重複を除いて並べる。
func (a *App) collectDeskNumbers(ctx context.Context, entries []map[string]any, project string) ([]int64, error) {
	var e error
	out := []int64{}
	seen := map[int64]bool{}
	for _, r := range entries {
		n, valid := positiveNumber(r["desk"])
		if !valid && project != "" && strings.EqualFold(project, fleet.Text(r["project"])) {
			if issue, ok := positiveNumber(r["gitea"]); ok {
				n, valid, e = a.Numbers.Number(ctx, project, issue)
				if e != nil {
					return nil, e
				}
			}
		}
		if valid && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

func (a *App) resolveMessageNumbers(ctx context.Context, d consultations.Record) ([]map[string]any, error) {
	messages := fleet.List(d["messages"])
	for i := len(messages) - 1; i >= 0; i-- {
		m := fleet.Object(messages[i])
		if m["role"] != "user" {
			continue
		}
		prior := objects(m["references"])
		complete := len(prior) > 0
		for _, r := range prior {
			complete = complete && fleet.Text(r["resolution"]) != ""
		}
		if complete {
			return references.LimitHistories(prior), nil
		}
		break
	}
	desks, e := a.referenceDesks(ctx, d)
	if e != nil {
		return nil, e
	}
	settings, configReadable := a.referenceSettings()
	out := []map[string]any{}
	for i, n := range desks {
		value := map[string]any{"desk": n, "resolution": "unknown"}
		if i >= 10 {
			value["resolution"], value["reason"] = "omitted", "1つのメッセージで解決する参照は10件までです。"
			value["historyUnread"] = true
			out = append(out, value)
			continue
		}
		mapped, found, e := a.Numbers.Issue(ctx, n)
		if e != nil {
			return nil, e
		}
		if found && a.Project(mapped.Project) == nil {
			issue, e := a.Gitea.Issue(ctx, mapped.Project, mapped.Number)
			if e != nil {
				value["reason"] = "参照先を取得できません"
				value["historyUnread"] = true
			} else if actual, ok := positiveNumber(issue["number"]); !ok || actual != mapped.Number {
				value["reason"] = "参照先の番号を確認できません"
				value["historyUnread"] = true
			} else {
				value["resolution"], value["project"], value["number"], value["title"], value["state"], value["body"] = "resolved", mapped.Project, mapped.Number, issue["title"], issue["state"], contract.ClipUTF16(consultations.MaskCredentials(fleet.Text(issue["body"])), 2000)
				for key, v := range a.referenceAcceptance(ctx, mapped.Project, issue, settings, configReadable) {
					value[key] = v
				}
				if issue["state"] == "closed" && a.precedingMerged(ctx, mapped.Project, mapped.Number) != nil {
					a.attachReferenceHistory(ctx, value, mapped.Project, mapped.Number)
				}
			}
		} else {
			value["reason"] = "許可されたプロジェクトの共通番号に対応しません"
		}
		out = append(out, value)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return references.LimitHistories(out), nil
}
func (a *App) attachReferenceHistory(ctx context.Context, value map[string]any, repo string, number int64) {
	if a.Issues == nil {
		value["historyUnread"] = true
		return
	}
	detail, e := a.Issues.Detail(ctx, repo, number, false, false)
	if e != nil {
		value["historyUnread"] = true
		return
	}
	comments, e := a.Gitea.Comments(ctx, repo, number)
	if e != nil {
		value["historyUnread"] = true
		return
	}
	text, ok := references.HistoryFromRecords(detail, comments, a.TrustedWriter)
	value["history"] = text
	if !ok {
		value["historyUnread"] = true
	}
}

func (a *App) resolveNumbers(ctx context.Context, d consultations.Record) (any, error) {
	messages := append([]any{}, fleet.List(d["messages"])...)
	last := -1
	for i, raw := range messages {
		if fleet.Object(raw)["role"] == "user" {
			last = i
		}
	}
	if last < 0 {
		return []map[string]any{}, nil
	}
	var current []map[string]any
	for i, raw := range messages {
		m := fleet.Object(raw)
		if m["role"] != "user" {
			continue
		}
		if i != last && len(fleet.List(m["references"])) == 0 {
			continue
		}
		copy := consultations.Record{}
		for key, v := range d {
			copy[key] = v
		}
		copy["messages"] = messages[:i+1]
		refs, e := a.resolveMessageNumbers(ctx, copy)
		if e != nil {
			return nil, e
		}
		next := map[string]any{}
		for key, v := range m {
			next[key] = v
		}
		next["references"] = refs
		messages[i] = next
		if i == last {
			current = refs
		}
	}
	d["messages"] = messages
	return current, nil
}
