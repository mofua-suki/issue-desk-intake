package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

func TestScanDoesNotRewriteUnchangedEmptyHeadReports(t *testing.T) {
	s, api, restart := adoptFixture(t)
	api.comments = nil
	api.issue["updated_at"] = "2026-11-01T00:00:00Z"
	api.issue["comments"] = 0
	mustScan(t, s)
	key := "review-followup:owner/repo#1"
	snapshot, _, err := s.records.Get(context.Background(), "proposal_scans", key)
	if err != nil {
		t.Fatal(err)
	}
	delete(snapshot, "headReportedComments")
	if err := s.saveScan(context.Background(), key, snapshot); err != nil {
		t.Fatal(err)
	}
	version := func() string {
		t.Helper()
		var value string
		if err := s.o.AppDB.QueryRow("SELECT xmin::text FROM proposal_scans WHERE id = $1", key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := version()
	restart()
	s = &Service{o: s.o, records: s.records}
	mustScan(t, s)
	if got := version(); got != initial {
		t.Fatalf("欠測したhead報告を空として扱わず保存しました: %s -> %s", initial, got)
	}
	api.issue["updated_at"] = "2026-11-01T00:01:00Z"
	mustScan(t, s)
	if got := version(); got == initial {
		t.Fatal("Issueの更新が保存されませんでした")
	}
}

// Exercise the shipping issueHistory/Comments decoder (json.Number counts and
// updated_at included), durable cursors, and manual merge without Issue updates.
func TestScanGiteaHistorySkipsUnchangedCommentsAcrossRestart(t *testing.T) {
	s, api, restart := adoptFixture(t)
	api.pull["merged"] = false
	reads := map[string]int{}
	updates := map[int]string{}
	commentsFail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.URL.Path == "/api/v1/repos/owner/repo/issues":
			issues := []Object{}
			for n := 1; n <= 30; n++ {
				stamp := "2026-11-01T00:00:00Z"
				if updates[n] != "" {
					stamp = updates[n]
				}
				issues = append(issues, Object{"number": n, "state": "closed", "updated_at": stamp, "comments": 1})
			}
			result = issues
		case r.URL.Path == "/api/v1/repos/owner/repo/issues/1/comments":
			result = api.comments
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if commentsFail {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			result = []any{}
		case r.URL.Path == "/api/v1/repos/owner/repo/pulls/2":
			result = api.pull
		case r.URL.Path == "/api/v1/repos/owner/repo/issues/1":
			result = api.issue
		case r.URL.Path == "/api/v1/repos/owner/repo/issues/comments/10":
			result = api.comments[0]
		default:
			t.Errorf("予期しない経路: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	connect := func() {
		t.Helper()
		client, err := gitea.New(server.URL, token, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.o.Gitea, s.o.Issues, s.o.Comments = client, client.Issues, client.Comments
	}
	connect()
	mustScan(t, s)
	count := func() int {
		total := 0
		for route, n := range reads {
			if strings.HasSuffix(route, "/comments") {
				total += n
			}
		}
		return total
	}
	if count() != 30 || api.posts != 0 {
		t.Fatal("初回走査", count(), api.posts)
	}
	mustScan(t, s)
	if count() != 30 || reads["/api/v1/repos/owner/repo/pulls/2"] != 2 {
		t.Fatal("未更新Issueのコメントを再取得、またはマージ待ちを再確認していません", reads)
	}
	restart()
	s = &Service{o: s.o, records: s.records}
	connect() // Also discard the Gitea client's transient issue cache.
	api.pull["merged"] = true
	mustScan(t, s)
	if count() != 30 || api.posts != 1 {
		t.Fatal("再起動後のカーソル・マージ待ち候補の復旧", count(), api.posts)
	}
	updates[2] = "2026-11-01T00:01:00Z"
	commentsFail = true
	if err := s.Scan(context.Background()); err == nil {
		t.Fatal("コメント読取失敗が無視されました")
	}
	commentsFail = false
	mustScan(t, s)
	mustScan(t, s)
	if count() != 32 || reads["/api/v1/repos/owner/repo/issues/2/comments"] != 3 || api.posts != 1 {
		// Issue 2: initial + failed + successful retry. The failure is included
		// in count, hence all collections total 32.
		t.Fatal("更新対象・読取失敗の再試行・カーソル", count(), reads, api.posts)
	}
}

func TestScanFilingGateDoesNotBlockDismiss(t *testing.T) {
	s, api, _ := adoptFixture(t)
	ctx := context.Background()
	// An independent rejected candidate is legitimately dismissible while the
	// scanner waits to file another candidate in the same source Issue.
	p := v3()
	p["id"] = "other-proposal"
	other := proposalComment(p, 11)
	api.comments = append(api.comments, other)
	parsed, fp, err := parse(str(other["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reject(ctx, "owner/repo", 1, other, parsed, fp, "保存済みの拒否"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	queue, err := jobs.New(s.o.AppDB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project,
		Own: func(u Object) bool { return u["login"] == "intake" }, ValidateFollowup: s.ValidateDraft,
		Acquire: func(ctx context.Context, _ string) (context.Context, func(), error) {
			close(entered)
			<-release
			return ctx, func() {}, nil
		},
		Check: s.ValidateDraft,
		Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
			return s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
		},
		Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) },
	})
	if err != nil {
		t.Fatal(err)
	}
	s.o.Submit = func(ctx context.Context, id string, d consultations.Record) (consultations.Record, error) {
		return filer.SubmitAccepted(ctx, id, d, auth.Principal{Kind: auth.Screen})
	}
	scanned := make(chan error, 1)
	go func() { scanned <- s.Scan(ctx) }()
	defer func() {
		close(release)
		if err := <-scanned; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("起票枠の待機へ進みません")
	}
	done := make(chan error, 1)
	go func() {
		input := Object{"project": "owner/repo", "number": 1, "commentId": 11, "proposalId": p["id"],
			"fingerprint": fp, "reason": "不要", "id": "00000000-0000-4000-8000-000000000001"}
		_, err := s.Dismiss(ctx, input, "screen", false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("走査の起票枠待機が破棄を止めています")
	}
}

func TestScanWaitsForNumberingWithoutRepeatingErrors(t *testing.T) {
	s, api, restart := adoptFixture(t)
	s.o.Desk = func(string, int64) any { return nil }
	api.issue["updated_at"] = "2026-11-01T00:00:00Z"
	reads := 0
	s.o.Comments = func(context.Context, string, int64) ([]Object, error) { reads++; return api.comments, nil }
	if err := s.Scan(context.Background()); err == nil {
		t.Fatal("共通番号の欠測が報告されません")
	}
	mustScan(t, s)
	restart()
	original := s // The fixture submitter retains this controller's options.
	s = &Service{o: s.o, records: s.records}
	mustScan(t, s) // Still missing: the durable warning must not repeat.
	s.o.Desk = func(string, int64) any { return int64(4553) }
	original.o.Desk = s.o.Desk
	mustScan(t, s)
	if reads != 1 || api.posts != 1 {
		t.Fatalf("採番待機中のコメント再取得=%d 起票=%d", reads, api.posts)
	}
}

func TestScanWorkerRepublishedHeadsAndBatches(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
			old, current, batch := v3(), v3(), v3()
			old["id"], current["id"], batch["id"] = workerFixtureID(a, "1"), workerFixtureID(b, "1"), workerFixtureID(b, "2")
			current["items"] = []any{Object{"problem": "同じ指摘の言い換え", "expected": "修正", "test": "検証"}}
			items := []any{}
			for i := 0; i < 5; i++ {
				items = append(items, Object{"problem": fmt.Sprintf("指摘%d", i), "expected": "修正", "test": "検証"})
			}
			batch["items"] = items
			comments := []Object{proposalComment(old, 10), proposalComment(current, 11), proposalComment(batch, 12)}
			if reverse {
				comments[0], comments[2] = comments[2], comments[0]
			}
			api.comments = comments
			api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:00:00Z", len(comments)
			reads := 0
			s.o.Comments = func(context.Context, string, int64) ([]Object, error) { reads++; return api.comments, nil }
			api.pull["merged"] = false
			mustScan(t, s)
			restart()
			s = &Service{o: s.o, records: s.records}
			api.pull["merged"] = true
			api.pull["head"] = Object{"sha": b}
			mustScan(t, s)
			firstRejection, err := s.read(context.Background(), "rejection:owner/repo#1:10")
			if err != nil || firstRejection == nil || str(firstRejection["checkedAt"]) == "" {
				t.Fatalf("初回の拒否: %v %v", firstRejection, err)
			}
			checkedAt := firstRejection["checkedAt"]
			previousNow := s.o.Now
			s.o.Now = func() time.Time { return previousNow().Add(time.Hour) }
			mustScan(t, s)
			for _, check := range []struct {
				id   string
				want bool
			}{{str(old["id"]), false}, {str(current["id"]), true}, {str(batch["id"]), true}} {
				mark, err := s.read(context.Background(), "owner/repo#1:"+check.id)
				if err != nil || (mark != nil) != check.want {
					t.Fatalf("%s: %v %v", check.id, mark, err)
				}
			}
			if api.posts != 2 || reads != 1 {
				t.Fatalf("posts=%d reads=%d", api.posts, reads)
			}
			rejection, err := s.read(context.Background(), "rejection:owner/repo#1:10")
			if err != nil || rejection["scope"] != "comment" || !strings.Contains(str(rejection["reason"]), "一致しません") || rejection["checkedAt"] != checkedAt || !reflect.DeepEqual(rejection, firstRejection) {
				t.Fatalf("不一致の拒否: %v %v", rejection, err)
			}
		})
	}
}

func TestScanReportsMissingHeadForEachComment(t *testing.T) {
	s, api, restart := adoptFixture(t)
	first, second := v3(), v3()
	first["id"] = workerFixtureID(strings.Repeat("a", 40), "1")
	second["id"] = workerFixtureID(strings.Repeat("b", 40), "1")
	api.comments = []Object{proposalComment(first, 10)}
	api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:00:00Z", 1
	if err := s.Scan(context.Background()); !errors.Is(err, errWorkerHead) || strings.Count(err.Error(), errWorkerHead.Error()) != 1 {
		t.Fatalf("最初の候補: %v", err)
	}
	mustScan(t, s)
	restart()
	s = &Service{o: s.o, records: s.records}
	api.comments = append(api.comments, proposalComment(second, 11))
	api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:01:00Z", 2
	if err := s.Scan(context.Background()); !errors.Is(err, errWorkerHead) || strings.Count(err.Error(), errWorkerHead.Error()) != 1 {
		t.Fatalf("追加候補: %v", err)
	}
	mustScan(t, s)
}

func TestScanWorkerHeadFailureRetainsCandidateAcrossRestart(t *testing.T) {
	for _, failure := range []string{"missing", "invalid", "sha256", "transport"} {
		t.Run(failure, func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			head := strings.Repeat("a", 40)
			p := v3()
			p["id"] = workerFixtureID(head, "1")
			api.comments = []Object{proposalComment(p, 10)}
			api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:00:00Z", 1
			if failure == "invalid" {
				api.pull["head"] = Object{"sha": 7}
			}
			if failure == "sha256" {
				api.pull["head"] = Object{"sha": strings.Repeat("a", 64)}
			}
			if failure == "transport" {
				api.before = func(route string) {
					if strings.Contains(route, "/pulls/") {
						api.err = errors.New("通信失敗")
					}
				}
			}
			if err := s.Scan(context.Background()); err == nil {
				t.Fatal("失敗が報告されません")
			}
			if failure != "transport" {
				mustScan(t, s)
			}
			api.err, api.before = nil, nil
			restart()
			s = &Service{o: s.o, records: s.records}
			if failure != "transport" {
				mustScan(t, s) // 報告済み状態は再起動後も保持される。
			}
			api.pull["head"] = Object{"sha": head}
			s.o.Comments = func(context.Context, string, int64) ([]Object, error) {
				t.Fatal("コメントを再取得しました")
				return nil, nil
			}
			mustScan(t, s)
			if api.posts != 1 {
				t.Fatalf("posts=%d", api.posts)
			}
		})
	}
}

func TestScanWaitsForConcurrentScanAndHonorsCancellation(t *testing.T) {
	s, _, _ := adoptFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	secondEntered := make(chan struct{})
	calls := 0
	s.o.Issues = func(ctx context.Context, repo, state string) ([]Object, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		} else if calls == 2 {
			close(secondEntered)
		}
		return nil, nil
	}
	first := make(chan error, 1)
	go func() { first <- s.Scan(context.Background()) }()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Scan(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan: %v", err)
	}
	second := make(chan error, 1)
	go func() { second <- s.Scan(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("concurrent scan returned before first finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second scan did not start")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestScanReconsidersSavedHeadRefusalsAfterRestart(t *testing.T) {
	s, api, restart := adoptFixture(t)
	old, posted, head := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	first, second, batch := v3(), v3(), v3()
	first["id"], second["id"], batch["id"] = workerFixtureID(old, "1"), workerFixtureID(posted, "1"), workerFixtureID(posted, "2")
	api.comments = []Object{proposalComment(first, 10), proposalComment(second, 11), proposalComment(batch, 12)}
	api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:00:00Z", 3
	api.pull["head"] = Object{"sha": head}
	api.commits = map[string][]Object{"/repos/owner/repo/pulls/2/commits?limit=50&page=1": {
		{"sha": old, "parents": []any{}}, {"sha": posted, "parents": []any{Object{"sha": old}}}, {"sha": head, "parents": []any{Object{"sha": posted}, Object{"sha": strings.Repeat("d", 40)}}},
	}}
	ctx := context.Background()
	for _, c := range api.comments {
		p, fp, err := parse(str(c["body"]))
		if err != nil {
			t.Fatal(err)
		}
		if err := saveOldHeadRejection(ctx, s, c, p, fp); err != nil {
			t.Fatal(err)
		}
	}
	key := "review-followup:owner/repo#1"
	if err := s.saveScan(ctx, key, map[string]any{"type": "review_followup_scan", "id": key, "updatedAt": api.issue["updated_at"], "commentCount": "3", "pending": []any{}}); err != nil {
		t.Fatal(err)
	}
	restart()
	s = &Service{o: s.o, records: s.records}
	mustScan(t, s)
	mustScan(t, s)
	for _, check := range []struct {
		id     int
		status string
	}{{10, "rejected"}, {11, "head_reconsidered"}, {12, "head_reconsidered"}} {
		r, err := s.read(ctx, fmt.Sprintf("rejection:owner/repo#1:%d", check.id))
		if err != nil || r["status"] != check.status || r["reason"] != oldHeadReason {
			t.Fatalf("rejection %d: %v %v", check.id, r, err)
		}
	}
	if api.posts != 2 {
		t.Fatalf("posts=%d", api.posts)
	}
}

func TestReconsiderationKeepsOtherAndChangedRefusals(t *testing.T) {
	for _, kind := range []string{"other", "proposal", "changed", "dismissed"} {
		t.Run(kind, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			ctx := context.Background()
			head := strings.Repeat("a", 40)
			p := v3()
			p["id"] = workerFixtureID(head, "1")
			c := proposalComment(p, 10)
			api.comments = []Object{c}
			api.pull["head"] = Object{"sha": head}
			parsed, fp, err := parse(str(c["body"]))
			if err != nil {
				t.Fatal(err)
			}
			reason := oldHeadReason
			if kind == "other" {
				reason = "別の理由"
			}
			if err := s.reject(ctx, "owner/repo", 1, c, parsed, fp, reason); err != nil {
				t.Fatal(err)
			}
			if reason == oldHeadReason {
				r, err := s.read(ctx, "rejection:owner/repo#1:10")
				if err != nil {
					t.Fatal(err)
				}
				delete(r, "headRuleVersion")
				if err := s.save(ctx, "rejection:owner/repo#1:10", r); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "proposal" {
				r, _ := s.read(ctx, "rejection:owner/repo#1:10")
				r["scope"] = "proposal"
				if err := s.save(ctx, "rejection:owner/repo#1:10", r); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "changed" {
				p["title"] = "変更された内容"
				api.comments = []Object{proposalComment(p, 10)}
			}
			if kind == "dismissed" {
				if err := s.save(ctx, "dismissal:owner/repo#1:"+str(parsed["id"]), Object{"type": "dismissal", "repo": "owner/repo", "number": 1, "id": parsed["id"], "status": "dismissed", "fingerprint": fp}); err != nil {
					t.Fatal(err)
				}
			}
			mustScan(t, s)
			if api.posts != 0 {
				t.Fatalf("保護された案を起票しました: %s", kind)
			}
		})
	}
}

func TestReconsideredRefusalUnknownOutcomeDoesNotRepost(t *testing.T) {
	s, api, restart := adoptFixture(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	p := v3()
	p["id"] = workerFixtureID(head, "1")
	c := proposalComment(p, 10)
	api.comments = []Object{c}
	api.pull["head"] = Object{"sha": head}
	parsed, fp, err := parse(str(c["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOldHeadRejection(ctx, s, c, parsed, fp); err != nil {
		t.Fatal(err)
	}
	sibling := v3()
	sibling["id"] = workerFixtureID(strings.Repeat("b", 40), "1")
	siblingComment := proposalComment(sibling, 11)
	siblingParsed, siblingFP, err := parse(str(siblingComment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOldHeadRejection(ctx, s, siblingComment, siblingParsed, siblingFP); err != nil {
		t.Fatal(err)
	}
	api.unknown = true
	connect := func() {
		queue, err := jobs.New(s.o.AppDB, jobs.Options{})
		if err != nil {
			t.Fatal(err)
		}
		filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(u Object) bool { return u["login"] == "intake" }, ValidateFollowup: s.ValidateDraft, ValidateNewPost: s.ValidateNewPost, Check: func(ctx context.Context, d consultations.Record) error { return s.ValidateDraft(ctx, d) }, Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
			return s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
		}, Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) }})
		if err != nil {
			t.Fatal(err)
		}
		s.o.Submit = func(ctx context.Context, id string, data consultations.Record) (consultations.Record, error) {
			return filer.SubmitAccepted(ctx, id, data, auth.Principal{Kind: auth.Screen})
		}
	}
	connect()
	if err := s.Scan(ctx); err == nil {
		t.Fatal("送信結果不明が成功扱いです")
	}
	restart()
	api.comments = append(api.comments, siblingComment)
	api.pull["head"] = Object{"sha": strings.Repeat("b", 40)}
	connect()
	_ = s.Scan(ctx)
	if api.posts != 1 {
		t.Fatalf("再POST: %d", api.posts)
	}
	d, found, err := s.records.Get(ctx, "drafts", draftID("owner/repo", 1, str(p["id"])))
	if err != nil || !found || d["status"] != "submitted" || d["issue"] == nil {
		t.Fatalf("復旧: %v %v", d, err)
	}
}

func TestReconsiderationHonorsConcurrentDismissal(t *testing.T) {
	s, api, _ := adoptFixture(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	p := v3()
	p["id"] = workerFixtureID(head, "1")
	c := proposalComment(p, 10)
	api.comments = []Object{c}
	api.pull["head"] = Object{"sha": head}
	parsed, fp, err := parse(str(c["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOldHeadRejection(ctx, s, c, parsed, fp); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	api.before = func(route string) {
		if route == "/repos/owner/repo/pulls/2" {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.Scan(ctx) }()
	<-entered
	input := Object{"project": "owner/repo", "number": 1, "commentId": 10, "proposalId": p["id"], "fingerprint": fp, "reason": "不要", "id": "00000000-0000-4000-8000-000000000001"}
	_, dismissErr := s.Dismiss(ctx, input, "screen", false)
	close(release)
	scanErr := <-done
	if dismissErr != nil || scanErr != nil || api.posts != 0 {
		t.Fatalf("dismiss=%v scan=%v posts=%d", dismissErr, scanErr, api.posts)
	}
	mustScan(t, s)
	if api.posts != 0 {
		t.Fatalf("再走査で起票: %d", api.posts)
	}
}

// Save the rejection shape produced before head-rule versioning.
func saveOldHeadRejection(ctx context.Context, s *Service, c, p Object, fp string) error {
	if err := s.reject(ctx, "owner/repo", 1, c, p, fp, oldHeadReason); err != nil {
		return err
	}
	key := fmt.Sprintf("rejection:owner/repo#1:%d", num(c["id"]))
	r, err := s.read(ctx, key)
	if err != nil {
		return err
	}
	delete(r, "headRuleVersion")
	return s.save(ctx, key, r)
}

func TestRejectedHeadDoesNotReviveAfterAnotherProposalIsFiled(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			ctx := context.Background()
			a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
			old, current := v3(), v3()
			old["id"], current["id"] = workerFixtureID(a, "1"), workerFixtureID(b, "1")
			api.comments = []Object{proposalComment(old, 10), proposalComment(current, 11)}
			api.pull["head"] = Object{"sha": b}
			if legacy {
				p, fp, err := parse(str(api.comments[0]["body"]))
				if err != nil {
					t.Fatal(err)
				}
				if err := saveOldHeadRejection(ctx, s, api.comments[0], p, fp); err != nil {
					t.Fatal(err)
				}
			}
			mustScan(t, s)
			if api.posts != 1 {
				t.Fatalf("初回起票数=%d", api.posts)
			}
			restart()
			s = &Service{o: s.o, records: s.records}
			api.pull["head"] = Object{"sha": a}
			mustScan(t, s)
			if api.posts != 1 {
				t.Fatalf("拒否済み案を再起票: %d", api.posts)
			}
			r, err := s.read(ctx, "rejection:owner/repo#1:10")
			if err != nil || r["status"] != "rejected" {
				t.Fatalf("拒否=%v err=%v", r, err)
			}
		})
	}
}

func TestReconsiderationRetriesCommitReadFailureAfterRestart(t *testing.T) {
	s, api, restart := adoptFixture(t)
	ctx := context.Background()
	posted, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	p := v3()
	p["id"] = workerFixtureID(posted, "1")
	c := proposalComment(p, 10)
	api.comments = []Object{c}
	api.issue["updated_at"], api.issue["comments"] = "2026-11-01T00:00:00Z", 1
	api.pull["head"] = Object{"sha": head}
	api.commits = map[string][]Object{"/repos/owner/repo/pulls/2/commits?limit=50&page=1": {
		{"sha": posted, "parents": []any{}}, {"sha": head, "parents": []any{Object{"sha": posted}, Object{"sha": strings.Repeat("c", 40)}}},
	}}
	parsed, fp, err := parse(str(c["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOldHeadRejection(ctx, s, c, parsed, fp); err != nil {
		t.Fatal(err)
	}
	api.commitErr = errors.New("一時的な一覧取得失敗")
	if err := s.Scan(ctx); err == nil || api.posts != 0 {
		t.Fatalf("一時失敗 err=%v posts=%d", err, api.posts)
	}
	r, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil || r["headRuleVersion"] != nil || r["status"] != "rejected" {
		t.Fatalf("一時失敗後の拒否=%v err=%v", r, err)
	}
	api.commitErr = nil
	restart()
	s = &Service{o: s.o, records: s.records}
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatalf("復旧後の起票=%d", api.posts)
	}
}

func TestLegacyRefusalDoesNotReviveAfterSiblingWasFiled(t *testing.T) {
	s, api, restart := adoptFixture(t)
	ctx := context.Background()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	old, current := v3(), v3()
	old["id"], current["id"] = workerFixtureID(a, "1"), workerFixtureID(b, "1")
	oldComment, currentComment := proposalComment(old, 10), proposalComment(current, 11)
	p, fp, err := parse(str(oldComment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOldHeadRejection(ctx, s, oldComment, p, fp); err != nil {
		t.Fatal(err)
	}
	api.comments = []Object{currentComment}
	api.pull["head"] = Object{"sha": b}
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatalf("初回起票数=%d", api.posts)
	}
	restart()
	s = &Service{o: s.o, records: s.records}
	api.comments = []Object{oldComment, currentComment}
	api.pull["head"] = Object{"sha": a}
	mustScan(t, s)
	if api.posts != 1 {
		t.Fatalf("旧拒否から二重起票: %d", api.posts)
	}
	r, err := s.read(ctx, "rejection:owner/repo#1:10")
	if err != nil || r["status"] != "rejected" {
		t.Fatalf("拒否=%v err=%v", r, err)
	}
}
