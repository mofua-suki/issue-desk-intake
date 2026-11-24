package proposals

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScanRecordsSpecificParseFailure(t *testing.T) {
	s, api, _ := adoptFixture(t)
	api.comments[0]["body"] = `<!-- issue-desk-proposal {invalid} -->`
	mustScan(t, s)
	r, err := s.read(context.Background(), "rejection:owner/repo#1:10")
	if err != nil || r["reason"] != "案のJSONを読めません。" {
		t.Fatalf("解析失敗の記録: %v, %v", r, err)
	}
}

func TestAdoptQuotedAndCopiedRefusalsDoNotPoisonWorkerProposal(t *testing.T) {
	for _, shape := range []string{"copy", "quote", "wrong-issue", "legacy-rejection"} {
		t.Run(shape, func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			api.issue["updated_at"] = "2026-11-01T00:00:00Z"
			api.pull["merged"] = false
			worker := api.comments[0]
			copy := proposalComment(v3(), 11)
			copy["user"] = Object{"login": "reader"}
			if shape == "quote" {
				copy["body"] = "> " + strings.ReplaceAll(str(copy["body"]), "\n", "\n> ")
			}
			if shape == "wrong-issue" {
				copy["user"] = Object{"login": "worker"}
				copy["issue_url"] = "https://git.example/api/v1/repos/owner/repo/issues/9"
			}
			api.comments = []Object{copy, worker} // Refusal is seen before the genuine proposal.
			mustScan(t, s)
			if api.posts != 0 {
				t.Fatal("未マージで起票されました")
			}
			rejected, err := s.read(context.Background(), "rejection:owner/repo#1:11")
			if err != nil || rejected["type"] != "rejection" || rejected["scope"] != "comment" {
				t.Fatal("引用・コピーの拒否記録がありません", rejected, err)
			}
			if shape == "legacy-rejection" {
				// Previous releases did not record a scope. They must also be
				// confined to their original comment instead of poisoning copies.
				delete(rejected, "scope")
				if err := s.save(context.Background(), "rejection:owner/repo#1:11", rejected); err != nil {
					t.Fatal(err)
				}
			}
			restart()
			s = &Service{o: s.o, records: s.records}
			api.pull["merged"] = true
			mustScan(t, s)
			mustScan(t, s)
			if api.posts != 1 {
				t.Fatal("正規の案が拒否または重複起票されました", api.posts)
			}
		})
	}
}

func TestAdoptExplicitContentRejectionAppliesAcrossComments(t *testing.T) {
	s, api, _ := adoptFixture(t)
	p, fp, err := parse(str(api.comments[0]["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(context.Background(), "rejection:owner/repo#1:11", Object{"type": "rejection", "scope": "proposal", "repo": "owner/repo", "number": 1,
		"id": p["id"], "fingerprint": fp, "commentId": 11, "status": "rejected"}); err != nil {
		t.Fatal(err)
	}
	mustScan(t, s)
	if api.posts != 0 {
		t.Fatal("内容を拒否した案が別コメントから起票されました")
	}
}

func TestScanPermanentPullMismatchIsRecordedOnce(t *testing.T) {
	s, api, restart := adoptFixture(t)
	p := v3()
	p["pr"] = "https://git.example/other/repo/pulls/2"
	api.comments[0] = proposalComment(p, 10)
	api.issue["updated_at"] = "2026-11-01T00:00:00Z"
	if err := s.Scan(context.Background()); err == nil {
		t.Fatal("初回の照合不能が報告されません")
	}
	r, err := s.read(context.Background(), "rejection:owner/repo#1:10")
	if err != nil || r["type"] != "rejection" || str(r["reason"]) == "" {
		t.Fatal("照合不能の記録がありません", r, err)
	}
	restart()
	s = &Service{o: s.o, records: s.records}
	mustScan(t, s)
	// Force rediscovery too: the same refusal must not emit another error.
	api.issue["updated_at"] = "2026-11-01T00:01:00Z"
	mustScan(t, s)
	if api.posts != 0 {
		t.Fatal("照合不能の案が起票されました")
	}
}

func TestMalformedCommentDismissalKeepsOtherAlerts(t *testing.T) {
	ctx := context.Background()
	s, api, restart := adoptFixture(t)
	malformed := proposalComment(v3(), 10)
	malformed["body"] = "<!-- issue-desk-proposal {bad} -->"
	malformed["updated_at"] = "2026-11-01T00:05:00Z"
	second := proposalComment(v3(), 11)
	second["body"] = `<!-- issue-desk-proposal {"version":3} -->`
	normal := proposalComment(v3(), 12)
	normal["user"] = Object{"login": "reader"}
	api.comments = []Object{malformed, second, normal}
	mustScan(t, s)
	alerts, err := s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != 3 {
		t.Fatalf("initial %v %v", alerts, err)
	}
	target := Object{"project": "owner/repo", "number": 1, "commentId": 10}
	candidate, err := s.Candidate(ctx, target)
	if err != nil || candidate["parseError"] != "案のJSONを読めません。" || candidate["body"] != malformed["body"] {
		t.Fatalf("candidate %v %v", candidate, err)
	}
	target["fingerprint"] = candidate["fingerprint"]
	target["id"] = "00000000-0000-4000-8000-000000000001"
	target["reason"] = "不要"
	target["instructionAt"] = "2026-11-01T00:04:00Z"
	if _, err := s.Dismiss(ctx, target, "token:pc", true); status(err) != 409 {
		t.Fatalf("old instruction %v", err)
	}
	target["instructionAt"] = "2026-11-01T00:05:00Z"
	malformed["body"] = "<!-- issue-desk-proposal {other} -->"
	if _, err := s.Dismiss(ctx, target, "token:pc", true); status(err) != 409 {
		t.Fatalf("changed body %v", err)
	}
	malformed["body"] = candidate["body"]
	mark, err := s.Dismiss(ctx, target, "token:pc", true)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.Dismiss(ctx, target, "token:pc", true)
	if err != nil || same["operationId"] != mark["operationId"] {
		t.Fatalf("retry %v %v", same, err)
	}
	other := Object{"project": "owner/repo", "number": 1, "commentId": 11, "id": target["id"], "reason": "不要", "fingerprint": target["fingerprint"]}
	if _, err := s.Dismiss(ctx, other, "screen", false); status(err) != 409 {
		t.Fatalf("operation ID reused across comments: %v", err)
	}
	target["reason"] = "別の理由"
	if _, err := s.Dismiss(ctx, target, "screen", false); status(err) != 409 {
		t.Fatalf("reused id %v", err)
	}
	mustScan(t, s)
	restart()
	alerts, err = s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("remaining alerts %v %v", alerts, err)
	}
	malformed["body"] = "<!-- issue-desk-proposal {new} -->"
	mustScan(t, s)
	alerts, err = s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != 3 {
		t.Fatalf("changed comment hidden %v %v", alerts, err)
	}
}

func TestMalformedReasonsReachAttentionAndLegacyInspection(t *testing.T) {
	ctx := context.Background()
	s, api, _ := fixture(t)
	if _, err := s.o.AppDB.ExecContext(ctx, "DELETE FROM proposals"); err != nil {
		t.Fatal(err)
	}
	valid := v3()
	items := []any{Object{"problem": "", "expected": "期待", "test": "検証"}}
	valid["items"] = items
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ body, reason string }{
		{"<!-- issue-desk-proposal broken -->", "案のマーカーの形式が不正です。"},
		{"<!-- issue-desk-proposal {bad} -->", "案のJSONを読めません。"},
		{"<!-- issue-desk-proposal {\"version\":3} -->", "案に不足または不明な項目があります。"},
		{"<!-- issue-desk-proposal " + string(raw) + " -->", "指摘・期待動作・検証方法が不正です。"},
	}
	for i, tc := range cases {
		c := Object{"id": 10 + i, "body": tc.body, "issue_url": api.comment["issue_url"], "user": api.comment["user"], "created_at": api.comment["created_at"]}
		if _, err := s.adopt(ctx, "owner/repo", 1, c); err != nil {
			t.Fatal(err)
		}
		record, err := s.read(ctx, fmt.Sprintf("rejection:owner/repo#1:%d", 10+i))
		if err != nil || record["reason"] != tc.reason {
			t.Fatalf("record %v %v", record, err)
		}
	}
	for _, field := range []string{"expected", "test"} {
		item := Object{"problem": "指摘", "expected": "期待", "test": "検証"}
		item[field] = ""
		proposal := v3()
		proposal["items"] = []any{item}
		encoded, err := json.Marshal(proposal)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct{ body, reason string }{"<!-- issue-desk-proposal " + string(encoded) + " -->", "指摘・期待動作・検証方法が不正です。"})
		c := Object{"id": 9 + len(cases), "body": cases[len(cases)-1].body, "issue_url": api.comment["issue_url"], "user": api.comment["user"], "created_at": api.comment["created_at"]}
		if _, err := s.adopt(ctx, "owner/repo", 1, c); err != nil {
			t.Fatal(err)
		}
	}
	alerts, err := s.UnfiledProposalAlerts(ctx)
	if err != nil || len(alerts) != len(cases) {
		t.Fatalf("alerts %v %v", alerts, err)
	}
	reasons := map[string]bool{}
	for _, a := range alerts {
		reasons[str(obj(a)["reason"])] = true
	}
	for _, tc := range cases {
		if !reasons[tc.reason] {
			t.Fatalf("missing reason %s: %v", tc.reason, alerts)
		}
	}
	old, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil {
		t.Fatal(err)
	}
	old["reason"] = "案の形式が不正です。"
	if err := s.save(ctx, "rejection:owner/repo#1:10", old); err != nil {
		t.Fatal(err)
	}
	api.comment["body"] = cases[0].body
	candidate, err := s.Candidate(ctx, Object{"project": "owner/repo", "number": 1, "commentId": 10})
	if err != nil || candidate["parseError"] != cases[0].reason || candidate["recordedReason"] != "案の形式が不正です（当時の詳細未保存）。" {
		t.Fatalf("legacy %v %v", candidate, err)
	}
}

type proposalAuthFixture struct{ delegated bool }

func (a *proposalAuthFixture) PCToken(_ context.Context, _ string) (auth.Token, bool, error) {
	return auth.Token{ID: "pc", Name: "pc", Delegated: a.delegated}, true, nil
}
func (*proposalAuthFixture) TouchPCToken(context.Context, string, time.Time) error { return nil }
func (*proposalAuthFixture) HostdToken(context.Context, string) (auth.Token, bool, error) {
	return auth.Token{}, false, nil
}
func (*proposalAuthFixture) Session(context.Context, string) (auth.Session, bool, error) {
	return auth.Session{}, false, nil
}
func (*proposalAuthFixture) ConsumeCLIKey(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

func TestMalformedCommentHTTPAuthorizationAndDismissal(t *testing.T) {
	s, api, _ := fixture(t)
	ctx := context.Background()
	r, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil {
		t.Fatal(err)
	}
	r["id"] = nil
	r["reason"] = "案の形式が不正です。"
	if err := s.save(ctx, "rejection:owner/repo#1:10", r); err != nil {
		t.Fatal(err)
	}
	api.comment["body"] = "<!-- issue-desk-proposal {bad} -->"
	store := &proposalAuthFixture{}
	s.o.Auth, err = auth.New(auth.Config{PublicURL: "https://desk.example", Port: 8787}, store)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://desk.example"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer idk_"+strings.Repeat("a", 43))
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	path := "/api/proposals/dismissal?project=owner%2Frepo&number=1&commentId=10"
	if w := call("GET", path, ""); w.Code != 403 {
		t.Fatalf("undelegated %d", w.Code)
	}
	store.delegated = true
	w := call("GET", path, "")
	if w.Code != 200 {
		t.Fatalf("inspect %d %s", w.Code, w.Body.String())
	}
	var c Object
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	input := Object{"project": "owner/repo", "number": 1, "commentId": 10, "id": "00000000-0000-4000-8000-000000000001", "fingerprint": c["fingerprint"], "reason": "不要", "instructionAt": "2026-11-01T00:04:00Z"}
	raw, _ := json.Marshal(input)
	if w = call("POST", "/api/proposals/dismiss", string(raw)); w.Code != 409 {
		t.Fatalf("old instruction %d %s", w.Code, w.Body.String())
	}
	input["instructionAt"] = "2026-11-01T00:05:00Z"
	raw, _ = json.Marshal(input)
	if w = call("POST", "/api/proposals/dismiss", string(raw)); w.Code != 200 {
		t.Fatalf("dismiss %d %s", w.Code, w.Body.String())
	}
}

func TestMalformedDismissalRejectsConcurrentAdoption(t *testing.T) {
	ctx := context.Background()
	s, api, _ := fixture(t)
	r, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil {
		t.Fatal(err)
	}
	r["id"] = nil
	if err = s.save(ctx, "rejection:owner/repo#1:10", r); err != nil {
		t.Fatal(err)
	}
	api.comment["body"] = "<!-- issue-desk-proposal {bad} -->"
	input := Object{"project": "owner/repo", "number": 1, "commentId": 10, "id": "00000000-0000-4000-8000-000000000001", "reason": "不要"}
	c, err := s.Candidate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input["fingerprint"] = c["fingerprint"]
	api.before = func() {
		api.before = nil
		if err := s.save(ctx, "owner/repo#1:adopted", Object{"type": "proposal", "repo": "owner/repo", "number": 1, "id": "adopted", "commentId": 10}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Dismiss(ctx, input, "screen", false); status(err) != 409 {
		t.Fatalf("adoption conflict: %v", err)
	}
}

func TestMalformedCandidateRejectsNowParseableComment(t *testing.T) {
	s, api, _ := adoptFixture(t)
	api.comments[0]["body"] = "<!-- issue-desk-proposal {bad} -->"
	mustScan(t, s)
	api.comments[0] = proposalComment(v3(), 10)
	_, err := s.Candidate(context.Background(), Object{"project": "owner/repo", "number": 1, "commentId": 10})
	if status(err) != 409 || !strings.Contains(fmt.Sprint(err), "元コメントは現在解析可能です。") {
		t.Fatalf("parseable comment: %v", err)
	}
}
