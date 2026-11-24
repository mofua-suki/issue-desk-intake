package screenapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"sort"
	"time"
)

func (s *Service) topicTable(ctx context.Context) error {
	_, e := s.o.AppDB.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS topic_link_removals(a TEXT NOT NULL,b TEXT NOT NULL,draft TEXT NOT NULL,other TEXT NOT NULL,at TEXT NOT NULL,token_name TEXT,PRIMARY KEY(a,b))`))
	return e
}
func pair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}
func (s *Service) removeTopicLink(ctx context.Context, id string, data Object, actor string) (Object, error) {
	other := fleet.Text(data["with"])
	if !uuid.MatchString(id) || !uuid.MatchString(other) || other == id {
		return nil, fail(400, "withに別の相談IDを指定してください。")
	}
	if e := s.topicTable(ctx); e != nil {
		return nil, e
	}
	tx, e := s.o.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var present int
	if e = tx.QueryRowContext(ctx, bindSQL(`SELECT count(*) FROM(SELECT id FROM drafts WHERE id=? UNION ALL SELECT id FROM todos WHERE id=?)`), id, id).Scan(&present); e != nil {
		return nil, e
	}
	if present == 0 {
		return nil, fail(404, "下書きが見つかりません。")
	}
	a, b := pair(id, other)
	if _, e = tx.ExecContext(ctx, insertIgnoreSQL("topic_link_removals", []string{"a", "b", "draft", "other", "at", "token_name"}, "VALUES(?,?,?,?,?,?)"), a, b, id, other, s.o.Now().UTC().Format(time.RFC3339Nano), actor); e != nil {
		return nil, e
	}
	for _, p := range [][2]string{{id, other}, {other, id}} {
		var raw string
		e = tx.QueryRowContext(ctx, bindSQL(`SELECT json FROM drafts WHERE id=?`), p[0]).Scan(&raw)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return nil, e
		}
		d := Object{}
		if e = json.Unmarshal([]byte(raw), &d); e != nil {
			return nil, e
		}
		withoutTopicLink(d, p[1])
		encoded, _ := json.Marshal(d)
		if _, e = tx.ExecContext(ctx, bindSQL(`UPDATE drafts SET json=? WHERE id=?`), string(encoded), p[0]); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return Object{"id": id, "with": other, "removed": true}, nil
}

// withoutTopicLink は相談の記録から、other とのつながりと重複の手がかりを外す。
func withoutTopicLink(d Object, other string) {
	links, hints := []any{}, []any{}
	for _, v := range fleet.Items(d["topicLinks"]) {
		if fleet.ObjectOrEmpty(v)["id"] != other {
			links = append(links, v)
		}
	}
	for _, v := range fleet.Items(d["duplicateHints"]) {
		label := fleet.Text(fleet.ObjectOrEmpty(v)["label"])
		if label != "下書き "+other && label != "TODO "+other {
			hints = append(hints, v)
		}
	}
	if len(links) > 0 {
		d["topicLinks"] = links
	} else {
		delete(d, "topicLinks")
	}
	if len(hints) > 0 {
		d["duplicateHints"] = hints
	} else {
		delete(d, "duplicateHints")
	}
}
func timeRange(q url.Values, now time.Time) (time.Time, time.Time, error) {
	to := now
	from := now.Add(-24 * time.Hour)
	var e error
	if v := q.Get("to"); v != "" {
		to, e = time.Parse(time.RFC3339Nano, v)
		if e != nil {
			return from, to, fail(400, "期間は時差付きISO日時で指定してください。")
		}
	}
	if v := q.Get("from"); v != "" {
		from, e = time.Parse(time.RFC3339Nano, v)
		if e != nil {
			return from, to, fail(400, "期間は時差付きISO日時で指定してください。")
		}
	} else {
		from = to.Add(-24 * time.Hour)
	}
	if !from.Before(to) || to.Sub(from) > 7*24*time.Hour {
		return from, to, fail(400, "期間は最長7日で指定してください。")
	}
	return from, to, nil
}
func (s *Service) topicLinks(ctx context.Context, q url.Values) (Object, error) {
	return s.topicLinksWithReader(ctx, q, s.topicRecords)
}

func (s *Service) topicLinksWithReader(ctx context.Context, q url.Values, read func(context.Context, string) (map[string]Object, error)) (Object, error) {
	from, to, e := timeRange(q, s.o.Now())
	if e != nil {
		return nil, e
	}
	if e = s.topicTable(ctx); e != nil {
		return nil, e
	}
	drafts, e := read(ctx, "drafts")
	if e != nil {
		return nil, e
	}
	todos, e := read(ctx, "todos")
	if e != nil {
		return nil, e
	}
	removed, e := s.removedTopicLinks(ctx)
	if e != nil {
		return nil, e
	}
	clusters, linkCounts := topicClusters(drafts, removed)
	used, reason := s.consultationUsage(ctx, from, to)
	groups := []Object{}
	for root, ids := range clusters {
		if len(ids) < 2 {
			continue
		}
		group, e := s.topicGroup(ctx, ids, linkCounts[root], drafts, todos, used)
		if e != nil {
			return nil, e
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, aok := groups[i]["weighted"].(float64)
		b, bok := groups[j]["weighted"].(float64)
		if !aok {
			a = -1
		}
		if !bok {
			b = -1
		}
		if a == b {
			return fleet.Text(groups[i]["id"]) < fleet.Text(groups[j]["id"])
		}
		return a > b
	})
	return Object{"from": from.UTC().Format(time.RFC3339Nano), "to": to.UTC().Format(time.RFC3339Nano), "groups": groups, "reason": reason}, nil
}

// removedTopicLinks は利用者が外した相談のつながりを読む。
func (s *Service) removedTopicLinks(ctx context.Context) (map[[2]string]bool, error) {
	var e error
	removed := map[[2]string]bool{}
	rows, e := s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT a,b FROM topic_link_removals`))
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var a, b string
		if e = rows.Scan(&a, &b); e != nil {
			rows.Close()
			return nil, e
		}
		removed[[2]string{a, b}] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return removed, nil
}

// topicClusters は相談のつながり（外したものを除く）でつながった相談の組と、組ごとのつながりの数を返す。
func topicClusters(drafts map[string]Object, removed map[[2]string]bool) (map[string][]string, map[string]int) {
	parents := map[string]string{}
	find := func(v string) string {
		if parents[v] == "" {
			parents[v] = v
		}
		root := v
		for parents[root] != root {
			root = parents[root]
		}
		parents[v] = root
		return root
	}
	edges := [][2]string{}
	for id, d := range drafts {
		for _, v := range fleet.Items(d["topicLinks"]) {
			other := fleet.Text(fleet.ObjectOrEmpty(v)["id"])
			if other == "" || other == id {
				continue
			}
			a, b := pair(id, other)
			if removed[[2]string{a, b}] {
				continue
			}
			edges = append(edges, [2]string{id, other})
			parents[find(id)] = find(other)
		}
	}
	clusters := map[string][]string{}
	for id := range parents {
		root := find(id)
		clusters[root] = append(clusters[root], id)
	}
	linkCounts := map[string]int{}
	for _, edge := range edges {
		linkCounts[find(edge[0])]++
	}
	return clusters, linkCounts
}

// consultationUsage は期間内の相談ごとの使用量を集める。取得できないときはその理由を返す。
func (s *Service) consultationUsage(ctx context.Context, from, to time.Time) (map[string]Object, any) {
	used := map[string]Object{}
	var reason any
	if s.o.Usage == nil {
		reason = "相談の使用量を取得する経路が設定されていません。"
	} else {
		usageRows, e := s.o.Usage(ctx, from, to)
		if e != nil {
			reason = "相談の使用量を取得できません。"
		} else {
			for _, r := range usageRows {
				if r["source"] != "intake" || fleet.Text(r["draft"]) == "" {
					continue
				}
				at, parseErr := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
				if parseErr != nil || at.Before(from) || !at.Before(to) {
					continue
				}
				id := fleet.Text(r["draft"])
				u := used[id]
				if u == nil {
					u = Object{"rows": 0, "weighted": nil, "missing": 0}
					used[id] = u
				}
				u["rows"] = intValue(u["rows"]) + 1
				if value, ok := usage.Weighted(r, s.o.UsageWeights)["weighted"].(float64); ok {
					previous, _ := u["weighted"].(float64)
					u["weighted"] = previous + value
				} else {
					u["missing"] = intValue(u["missing"]) + 1
				}
			}
		}
	}
	return used, reason
}

// topicGroup はつながった相談の組1つについて、各相談の結果・共通番号・使用量をまとめる。
func (s *Service) topicGroup(ctx context.Context, ids []string, links int, drafts, todos map[string]Object, used map[string]Object) (Object, error) {
	sort.Strings(ids)
	counts := Object{"members": len(ids), "drafts": 0, "todos": 0, "finished": 0, "missing": 0}
	outcomes := Object{}
	desks := map[int64]bool{}
	members := []any{}
	var weighted any
	missing := 0
	for _, id := range ids {
		d, t := drafts[id], todos[id]
		state, outcome := "missing", "unknown"
		if d != nil {
			state = "draft"
			outcome = "draft"
			if d["status"] == "submitted" {
				outcome = "filed"
				if d["finishedWithoutIssue"] == true || d["issue"] == nil && len(fleet.Items(d["issues"])) == 0 {
					outcome = "finished"
				}
			}
		} else if t != nil {
			state, outcome = "todo", "todo"
		}
		resolved := Object{}
		if s.o.Outcome != nil {
			var e error
			resolved, e = s.o.Outcome(ctx, id)
			if e != nil {
				return nil, e
			}
			if fleet.Text(resolved["outcome"]) != "" {
				outcome = fleet.Text(resolved["outcome"])
			}
		}
		key := "missing"
		if d != nil {
			key = "drafts"
			if outcome == "finished" {
				key = "finished"
			}
		} else if t != nil {
			key = "todos"
		}
		counts[key] = intValue(counts[key]) + 1
		outcomes[outcome] = intValue(outcomes[outcome]) + 1
		for _, n := range fleet.Items(resolved["desks"]) {
			if v, ok := safeNumber(n); ok {
				desks[v] = true
			}
		}
		u := used[id]
		if value, ok := u["weighted"].(float64); ok {
			previous, _ := weighted.(float64)
			weighted = previous + value
		}
		missing += intValue(u["missing"])
		members = append(members, Object{"id": id, "state": state, "outcome": outcome, "weighted": u["weighted"]})
	}
	deskValues := []int64{}
	for n := range desks {
		deskValues = append(deskValues, n)
	}
	sort.Slice(deskValues, func(i, j int) bool { return deskValues[i] < deskValues[j] })
	return Object{"id": ids[0], "counts": counts, "links": links, "filed": intValue(outcomes["filed"])+intValue(outcomes["investigation"]) > 0, "finished": intValue(outcomes["finished"]) > 0, "outcomes": outcomes, "desks": deskValues, "weighted": weighted, "missing": missing, "members": members}, nil
}

func intValue(v any) int { return int(fleet.Int64(v)) }
