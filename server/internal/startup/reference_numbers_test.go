package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/references"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestPostgresReferencesResolveQualifiedNumbersHistoryAndCurrentAcceptance(t *testing.T) {
	pg := appPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	local := newAppStore(t)
	records, e := consultations.OpenRecords(ctx, local.DB())
	if e != nil {
		t.Fatal(e)
	}
	index, e := numbers.OpenLedger(ctx, local.DB(), filepath.Join(dir, "intake-desk-numbers.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	queue, e := jobs.New(pg, jobs.Options{})
	if e != nil {
		t.Fatal(e)
	}
	issue := func(n int, state string) map[string]any {
		return map[string]any{"number": n, "title": fmt.Sprintf("依頼%d", n), "body": "依頼の本文", "state": state, "user": map[string]any{"id": 1, "login": "intake"}, "created_at": "2026-11-01T00:00:00Z", "closed_at": "2026-11-01T01:00:00Z"}
	}
	issues := map[string]map[string]any{"7": issue(7, "closed"), "8": issue(8, "open"), "9": issue(9, "closed"), "10": issue(10, "closed")}
	issues["7"]["body"] = "旧本文の受入条件: 周期を短縮する"
	candidates := []numbers.Candidate{}
	for n := int64(7); n <= 10; n++ {
		candidates = append(candidates, numbers.Candidate{Number: n, CreatedAt: "2026-11-01T00:00:00Z"})
	}
	if _, e = index.Assign(ctx, []numbers.List{{Project: "owner/repo", Issues: candidates}}, []string{"owner/repo"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e = pg.ExecContext(ctx, `INSERT INTO jobs(repo,issue,title,status,generation,result,view,pr)VALUES('owner/repo',7,'failed','failed',0,'{"error":"token=secret failure"}','{"reviewHolds":0}',NULL),('owner/repo',8,'limited','failed',1,'{}','{"reviewHolds":0}',NULL),('owner/repo',10,'merged','merged',0,'{}','{"reviewHolds":0}',20)`); e != nil {
		t.Fatal(e)
	}
	listStatus := 200
	unread7 := false
	commentsCalls := 0
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		body := "{}"
		status := 200
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/issues"):
			status = listStatus
			if status != 200 {
				body = "{}"
				break
			}
			if r.URL.Query().Get("state") == "open" {
				raw, _ := json.Marshal([]any{issues["8"]})
				body = string(raw)
			} else {
				rows := []any{}
				for _, n := range []string{"7", "8", "9", "10"} {
					rows = append(rows, issues[n])
				}
				raw, _ := json.Marshal(rows)
				body = string(raw)
			}
		case strings.HasSuffix(path, "/comments"):
			commentsCalls++
			if strings.Contains(path, "/issues/9/") || unread7 {
				status = 500
				break
			}
			raw, _ := json.Marshal(append([]map[string]any{referenceDecision(1, "仕様の不整合を指摘\n<!-- gcr:owner/repo#7:review-hold:1 -->")}, referenceDecisions()...))
			body = string(raw)
		case strings.HasSuffix(path, "/pulls/20"):
			body = `{"merged":true,"base":{"repo":{"full_name":"owner/repo"},"ref":"main"}}`
		case strings.HasSuffix(path, "/repos/owner/repo"):
			body = `{"default_branch":"main"}`
		default:
			parts := strings.Split(path, "/")
			if row := issues[parts[len(parts)-1]]; row != nil {
				raw, _ := json.Marshal(row)
				body = string(raw)
			} else {
				status = 404
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	fleetFile, assignFile := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "assignments.json")
	writeConfig(t, fleetFile, map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	assign := map[string]any{"pollSeconds": 300, "implementationRunLimit": 2, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}}
	writeConfig(t, assignFile, assign)
	a := App{ctx: ctx, Config: &Config{Values: map[string]any{"workerId": "nas", "giteaUrl": "http://gitea.invalid", "gitUser": "owner", "repositories": []any{map[string]any{"name": "owner/repo"}}}, Repositories: []string{"owner/repo"}}, DB: pg, SQLite: local, Records: records, Numbers: index, Jobs: queue, Gitea: api, Fleet: &fleet.Service{DB: pg, FleetPath: fleetFile, AssignmentsPath: assignFile}, ownIDs: map[int64]bool{1: true}, writerIDs: map[int64]bool{2: true}, intakeID: 1, Log: func(string) {}}
	a.Issues, e = issueapi.Register(http.NewServeMux(), issueapi.Options{AppDB: local.DB(), Postgres: pg, Auth: &auth.Authenticator{}, Gitea: api, Numbers: index, Jobs: queue, Project: a.Project, Own: a.Own, DedicatedAuthor: a.DedicatedAuthor, ConfigReader: a.Values})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Issues.Close()
	d := consultations.Record{"project": "owner/repo", "messages": []any{map[string]any{"role": "user", "text": "OWNER/REPO#7 owner/repo#9 owner/repo#10 other/repo#7"}}}
	value, e := a.resolveNumbers(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	refs := value.([]map[string]any)
	if len(refs) != 3 {
		t.Fatal(refs)
	}
	byNumber := map[int64]map[string]any{}
	for _, r := range refs {
		byNumber[int64(fleet.Number(r["number"]))] = r
	}
	if !strings.Contains(fleet.Text(byNumber[7]["history"]), "仕様の不整合を指摘") || strings.Contains(fleet.Text(byNumber[7]["history"]), "secret") || byNumber[9]["historyUnread"] != true || byNumber[10]["history"] != nil || byNumber[10]["historyUnread"] != nil {
		t.Fatal(refs)
	}
	history := fleet.Text(byNumber[7]["history"])
	for _, want := range []string{"ID=136239", "ID=145889", "1-Aで良い", "周期の短縮は取り下げ、即時反映と理由の表示だけにする", "1-Aを維持", "fixture-author", "作成=", "更新="} {
		if !strings.Contains(history, want) {
			t.Fatalf("後続判断と出典が届かない: %q: %s", want, history)
		}
	}
	if byNumber[7]["body"] != "旧本文の受入条件: 周期を短縮する" || byNumber[7]["historyUnread"] == true {
		t.Fatal(byNumber[7])
	}
	d["references"] = refs
	if _, e = records.Change(ctx, "drafts", "reference-fixture", func(*sql.Tx, consultations.Record) (consultations.Record, error) { return d, nil }); e != nil {
		t.Fatal(e)
	}
	saved, found, e := records.Get(ctx, "drafts", "reference-fixture")
	if e != nil || !found {
		t.Fatal(e, found)
	}
	savedHistory := fleet.Text(objects(fleet.Object(fleet.List(saved["messages"])[0])["references"])[0]["history"])
	if savedHistory != history || fleet.Text(objects(saved["references"])[0]["history"]) != history {
		t.Fatal(saved)
	}
	calls := commentsCalls
	unread7 = true
	if _, e = a.resolveNumbers(ctx, d); e != nil || commentsCalls != calls {
		t.Fatal("保存済みの参照を再起動・再試行で取り直した", commentsCalls, calls, e)
	}
	unread7 = false
	text, e := a.openRequests(ctx, consultations.Record{"project": "owner/repo"})
	if e != nil || !strings.Contains(text, "追加の指示: 受け付けない") || !strings.Contains(text, "実装の回数の上限") {
		t.Fatal(text, e)
	}
	assign["implementationRunLimit"] = 4
	writeConfig(t, assignFile, assign)
	text, e = a.openRequests(ctx, consultations.Record{"project": "owner/repo"})
	if e != nil || strings.Contains(text, "追加の指示: 受け付けない") {
		t.Fatal("変更後の現設定を反映しない", text, e)
	}
	legacy := consultations.Record{"project": "owner/repo", "messages": []any{map[string]any{"role": "user", "text": "参照の続き", "references": []any{map[string]any{"project": "OWNER/REPO", "gitea": float64(7)}}}}}
	legacyRefs, e := a.resolveNumbers(ctx, legacy)
	if e != nil || len(legacyRefs.([]map[string]any)) != 1 {
		t.Fatal("保存済みrepo形式を解決しない", legacyRefs, e)
	}
	listStatus = 503
	pending := consultations.Record{"project": "owner/repo", "messages": []any{map[string]any{"role": "user", "text": "owner/repo#99"}}}
	if _, e = a.resolveNumbers(ctx, pending); e == nil {
		t.Fatal("一時的な対応表失敗を不在として確定")
	}
	listStatus = 404
	desk7, _, e := index.Number(ctx, "owner/repo", 7)
	if e != nil {
		t.Fatal(e)
	}
	pending = consultations.Record{"project": "owner/repo", "messages": []any{map[string]any{"role": "user", "text": fmt.Sprintf("owner/repo#99 と共通番号%d", desk7)}}}
	partial, e := a.resolveNumbers(ctx, pending)
	if e != nil || len(partial.([]map[string]any)) != 1 {
		t.Fatal("恒久的な対応表失敗で既知の参照まで捨てた", partial, e)
	}

}

// Reported IDs, creation times and excerpts are the issue's regression subject.
// Authors and update times are explicitly synthetic; no real provenance is inferred.
func referenceDecision(id int, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "created_at": "2026-11-02T20:59:50Z", "updated_at": "2026-11-07T00:00:00Z", "user": map[string]any{"id": 2, "login": "fixture-author"}}
}
func referenceDecisions() []map[string]any {
	withdrawal := referenceDecision(136239, "1-Aで良い。周期の短縮は取り下げ、即時反映と理由の表示だけにする")
	maintenance := referenceDecision(145889, "1-Aを維持してください。")
	maintenance["created_at"], maintenance["updated_at"] = "2026-11-06T11:01:07Z", "2026-11-06T11:01:07Z"
	return []map[string]any{maintenance, withdrawal}
}

func TestPostgresReferenceHistoryOmissionsReachSavedUserReferences(t *testing.T) {
	ctx := context.Background()
	pg, local := appPostgres(t), newAppStore(t)
	records, e := consultations.OpenRecords(ctx, local.DB())
	if e != nil {
		t.Fatal(e)
	}
	index, e := numbers.OpenLedger(ctx, local.DB(), filepath.Join(t.TempDir(), "numbers-desk-numbers.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	queue, e := jobs.New(pg, jobs.Options{})
	if e != nil {
		t.Fatal(e)
	}
	candidates := []numbers.Candidate{}
	for n := int64(1); n <= 12; n++ {
		candidates = append(candidates, numbers.Candidate{Number: n, CreatedAt: "2026-11-01T00:00:00Z"})
	}
	if _, e = index.Assign(ctx, []numbers.List{{Project: "owner/repo", Issues: candidates}}, []string{"owner/repo"}, true); e != nil {
		t.Fatal(e)
	}
	mode := ""
	commentsCalls := 0
	calls := 0
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		calls++
		status, body := 200, "{}"
		parts := strings.Split(r.URL.Path, "/")
		if strings.HasSuffix(r.URL.Path, "/comments") {
			commentsCalls++
			n := parts[len(parts)-2]
			rows := referenceDecisions()
			switch mode {
			case "failed-comments":
				status = 500
			case "late-comments-failure":
				if commentsCalls >= 2 {
					status = 500
				}
			case "malformed-response":
				body = `{"not":"a comment list"}`
			case "missing-source":
				delete(rows[0], "updated_at")
			case "long-comment":
				rows[0]["body"] = strings.Repeat("長文", 2000)
			case "long-source":
				rows[0]["user"].(map[string]any)["login"] = strings.Repeat("出典", 2000)
			case "input-limit":
				rows = []map[string]any{}
			case "total-budget":
				rows = []map[string]any{referenceDecision(1, strings.Repeat("文", 1200)+"この参照="+n)}
			}
			if mode != "malformed-response" {
				raw, _ := json.Marshal(rows)
				body = string(raw)
			}
		} else if strings.Contains(r.URL.Path, "/issues/") {
			var n int
			fmt.Sscan(parts[len(parts)-1], &n)
			if mode == "failed-issue" {
				status = 500
			}
			if mode == "wrong-number" {
				n++
			}
			raw, _ := json.Marshal(map[string]any{"number": n, "title": "固定参照", "body": "旧本文の受入: 周期を短縮 token=fixture-secret", "state": "closed", "user": map[string]any{"id": 1, "login": "intake"}, "created_at": "2026-11-01T00:00:00Z", "closed_at": "2026-11-01T01:00:00Z"})
			body = string(raw)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	dir := t.TempDir()
	fleetFile, assignFile := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "assignments.json")
	writeConfig(t, fleetFile, map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, assignFile, map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	a := App{ctx: ctx, Config: &Config{Values: map[string]any{"workerId": "nas", "gitUser": "owner", "repositories": []any{map[string]any{"name": "owner/repo"}}}, Repositories: []string{"owner/repo"}}, DB: pg, SQLite: local, Records: records, Numbers: index, Jobs: queue, Gitea: api, Fleet: &fleet.Service{DB: pg, FleetPath: fleetFile, AssignmentsPath: assignFile}, ownIDs: map[int64]bool{1: true}, writerIDs: map[int64]bool{2: true}, intakeID: 1, Log: func(string) {}}
	a.Issues, e = issueapi.Register(http.NewServeMux(), issueapi.Options{AppDB: local.DB(), Postgres: pg, Auth: &auth.Authenticator{}, Gitea: api, Numbers: index, Jobs: queue, Project: a.Project, Own: a.Own, DedicatedAuthor: a.DedicatedAuthor, ConfigReader: a.Values})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Issues.Close()
	for _, tc := range []struct {
		name, source string
		unread       int
	}{
		{"failed-comments", "owner/repo#1", 1},
		{"late-comments-failure", "owner/repo#1", 1},
		{"wrong-number", "owner/repo#1", 1},
		{"failed-issue", "owner/repo#1", 1},
		{"malformed-response", "owner/repo#1", 1},
		{"missing-source", "owner/repo#1", 1},
		{"long-comment", "owner/repo#1", 1},
		{"long-source", "owner/repo#1", 1},
		{"total-budget", "owner/repo#1 owner/repo#2 owner/repo#3 owner/repo#4 owner/repo#5 owner/repo#6", 2},
		{"input-limit", "owner/repo#1 owner/repo#2 owner/repo#3 owner/repo#4 owner/repo#5 owner/repo#6 owner/repo#7 owner/repo#8 owner/repo#9 owner/repo#10 owner/repo#11 owner/repo#12", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode = tc.name
			commentsCalls = 0
			d := consultations.Record{"project": "owner/repo", "messages": []any{map[string]any{"role": "user", "text": tc.source}}}
			result, err := a.resolveNumbers(ctx, d)
			if err != nil {
				t.Fatal(err)
			}
			refs := result.([]map[string]any)
			total := 0
			for i, r := range refs {
				text := fleet.Text(r["history"])
				total += len([]rune(text))
				if (mode == "total-budget" && (r["historyUnread"] == true) != (i >= 4)) || (mode == "input-limit" && (r["historyUnread"] == true) != (i >= 10)) {
					t.Fatal("省略対象の参照を誤った", i, r)
				}
				if len([]rune(text)) > 1500 {
					t.Fatal(r)
				}
				if mode == "total-budget" && i < 4 && (!strings.Contains(text, fmt.Sprintf("この参照=%d", i+1)) || strings.Contains(text, fmt.Sprintf("この参照=%d", (i+1)%6+1))) {
					t.Fatal("別Issueの経緯を混ぜた", r)
				}
			}
			unread := references.UnreadHistoryDesks(d)
			if total > 6000 || len(unread) != tc.unread {
				t.Fatal(total, unread, refs)
			}
			encoded, _ := json.Marshal(d)
			if strings.Contains(string(encoded), "fixture-secret") {
				t.Fatal("資格情報が参照へ漏れた")
			}
			if _, err = records.Change(ctx, "drafts", tc.name, func(*sql.Tx, consultations.Record) (consultations.Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			saved, _, err := records.Get(ctx, "drafts", tc.name)
			if err != nil || len(references.UnreadHistoryDesks(saved)) != tc.unread {
				t.Fatal(saved, err)
			}
			before := calls
			if _, err = a.resolveNumbers(ctx, saved); err != nil {
				t.Fatal(err)
			}
			// Completed resolution attempts, including unread and omitted results, are reused.
			if calls != before {
				t.Fatal("解決済み参照を再取得", calls, before)
			}
		})
	}
}

func TestOpenRequestsContinuationChangedList(t *testing.T) {
	ctx := context.Background()
	local := newAppStore(t)
	records, err := consultations.OpenRecords(ctx, local.DB())
	if err != nil {
		t.Fatal(err)
	}
	index, err := numbers.OpenLedger(ctx, local.DB(), filepath.Join(t.TempDir(), "numbers-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	// 一覧の末尾へ全件を載せきれない件数にして、続き位置が残る経路を確かめる。
	for i := 0; i < 500; i++ {
		rows = append(rows, map[string]any{"number": i + 1, "title": "SQLite", "body": strings.Repeat("文", 300), "created_at": fmt.Sprintf("%04d", 500-i), "user": map[string]any{"id": 1}})
	}
	fail := false
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		status := 200
		if fail {
			status = 503
		}
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 || r.URL.Query().Get("limit") != "50" {
			t.Fatalf("invalid pagination: %s", r.URL.RawQuery)
		}
		start := (page - 1) * 50
		if start > len(rows) {
			start = len(rows)
		}
		end := start + 50
		if end > len(rows) {
			end = len(rows)
		}
		raw, _ := json.Marshal(rows[start:end])
		return &http.Response{StatusCode: status, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	a := App{ctx: ctx, Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api, Records: records, Numbers: index, ownIDs: map[int64]bool{1: true}}
	draft := consultations.Record{"project": "owner/repo", "title": "SQLite"}
	initial, err := a.reference(ctx, draft, "open-requests")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(initial.Text)
	if len(match) != 2 {
		t.Fatal(initial.Text)
	}
	if !strings.Contains(initial.Text, "重なり候補の続きは readFiles") {
		t.Fatal("continuation instructions missing")
	}
	page, err := a.referenceContinuation(ctx, draft, match[1])
	if err != nil || !strings.Contains(page.Text, "owner/repo#") {
		t.Fatalf("page: %s %v", page.Text, err)
	}
	rows[0]["title"] = "変更"
	if page, err = a.referenceContinuation(ctx, draft, match[1]); !errors.Is(err, references.ErrOpenListChanged) || page.Text != "" {
		t.Fatalf("changed list: %+v %v", page, err)
	}
	latest, err := a.reference(ctx, draft, "open-requests")
	if err != nil {
		t.Fatal(err)
	}
	newMatch := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindStringSubmatch(latest.Text)
	if len(newMatch) != 2 {
		t.Fatal(latest.Text)
	}
	for _, cursor := range []string{"bad", strings.Split(newMatch[1], ":")[0] + ":0:9999", strings.Split(newMatch[1], ":")[0] + ":9:1"} {
		if page, err = a.referenceContinuation(ctx, draft, cursor); err == nil || errors.Is(err, references.ErrOpenListChanged) || page.Text != "" {
			t.Fatalf("invalid cursor %q: %+v %v", cursor, page, err)
		}
	}
	fail = true
	if _, err = a.referenceContinuation(ctx, draft, match[1]); err == nil || errors.Is(err, references.ErrOpenListChanged) {
		t.Fatal("fetch failure misclassified", err)
	}
	fail = false
	rows = rows[:1]
	short, err := a.reference(ctx, draft, "open-requests")
	if err != nil || strings.Contains(short.Text, "重なり候補の続きは readFiles") {
		t.Fatalf("unexpected continuation instructions: %s %v", short.Text, err)
	}
}
