package screenapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestTopicProjectionMatchesLegacy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	insert := func(table, id, raw string) {
		t.Helper()
		if _, err := f.db.DB().Exec("INSERT INTO "+table+"(id,json) VALUES(?,?)", id, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert("drafts", "a", `{"id":"different","topicLinks":[{"id":"b"},{"id":"missing"},{"id":"gone"}],"status":"submitted","issue":{"number":1}}`)
	insert("drafts", "b", `{"topicLinks":[{"id":"a"},{"id":"todo"}],"status":"submitted","finishedWithoutIssue":true}`)
	insert("drafts", "gone", `{"topicLinks":[{"id":"a"}]}`)
	insert("drafts", "odd", `{"topicLinks":[null,1,{"id":false}],"status":null,"issues":{},"context":"\u0000"}`)
	insert("drafts", "null", `null`)
	insert("todos", "todo", `{"id":"wrong","context":"ordinary"}`)
	insert("drafts", "oddnormal", `{"topicLinks":[null,1,{"id":"todo"}],"status":null,"issues":{}}`)
	insert("drafts", "c", `{"topicLinks":[{"id":"d"},{"id":"e"}],"status":"submitted","issue":{"number":2}}`)
	insert("drafts", "d", `{"status":"submitted","issues":[]}`)
	insert("drafts", "e", `{"status":5}`)
	for _, item := range []struct {
		table, id string
		fallback  bool
	}{
		{"drafts", "oddnormal", false}, {"drafts", "odd", true}, {"todos", "todo", false},
	} {
		var fallback bool
		if err := f.db.DB().QueryRow("SELECT topic_summary IS NULL FROM "+item.table+" WHERE id=?", item.id).Scan(&fallback); err != nil {
			t.Fatal(err)
		}
		if fallback != item.fallback {
			t.Fatalf("%s/%s: fallback=%v", item.table, item.id, fallback)
		}
	}
	if err := f.s.topicTable(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().Exec(`INSERT INTO topic_link_removals(a,b,draft,other,at) VALUES('a','gone','a','gone','now')`); err != nil {
		t.Fatal(err)
	}
	f.s.o.Usage = func(context.Context, time.Time, time.Time) ([]Object, error) {
		return []Object{{"source": "intake", "draft": "a", "at": "2026-10-31T23:00:00Z"}}, nil
	}
	f.s.o.Outcome = func(_ context.Context, id string) (Object, error) {
		if id == "todo" {
			return Object{"outcome": "investigation", "desks": []any{float64(42)}}, nil
		}
		return Object{}, nil
	}
	before, err := f.s.legacyTopicLinks(ctx, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.s.topicLinks(ctx, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy=%#v\nnew=%#v", before, after)
	}
	groups := after["groups"].([]Object)
	if len(groups) != 2 || groups[0]["links"] != 5 || groups[0]["missing"] != 1 || groups[1]["links"] != 2 {
		t.Fatalf("unexpected graph: %#v", groups)
	}
	for _, raw := range []string{`{"bad":1e400}`, `{"bad":`, `[]`, `{"bad":"\ud800"}`, `{"bad":1e400,"bad":0}`, `{"nested":[{"bad":1e400}]}`} {
		if _, err := f.db.DB().Exec(`UPDATE drafts SET json=? WHERE id='odd'`, raw); err != nil {
			t.Fatal(err)
		}
		old, e1 := f.s.records(ctx, "drafts")
		got, e2 := f.s.topicRecords(ctx, "drafts")
		if (e1 == nil) != (e2 == nil) {
			t.Fatalf("%s: legacy=%v new=%v", raw, e1, e2)
		}
		if e1 == nil && !reflect.DeepEqual(old["odd"], got["odd"]) {
			t.Fatalf("fallback mismatch %s: %#v %#v", raw, old["odd"], got["odd"])
		}
	}
}

// legacyTopicLinks preserves the original full-record reader and per-cluster edge scan.
func (s *Service) legacyTopicLinks(ctx context.Context, q url.Values) (Object, error) {
	from, to, e := timeRange(q, s.o.Now())
	if e != nil {
		return nil, e
	}
	if e = s.topicTable(ctx); e != nil {
		return nil, e
	}
	drafts, e := s.records(ctx, "drafts")
	if e != nil {
		return nil, e
	}
	todos, e := s.records(ctx, "todos")
	if e != nil {
		return nil, e
	}
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
	groups := []Object{}
	for root, ids := range clusters {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		counts := Object{"members": len(ids), "drafts": 0, "todos": 0, "finished": 0, "missing": 0}
		outcomes := Object{}
		desks := map[int64]bool{}
		members := []any{}
		var weighted any
		missing, links := 0, 0
		for _, edge := range edges {
			if find(edge[0]) == root {
				links++
			}
		}
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
		groups = append(groups, Object{"id": ids[0], "counts": counts, "links": links, "filed": intValue(outcomes["filed"])+intValue(outcomes["investigation"]) > 0, "finished": intValue(outcomes["finished"]) > 0, "outcomes": outcomes, "desks": deskValues, "weighted": weighted, "missing": missing, "members": members})
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
