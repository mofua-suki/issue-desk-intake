package proposals

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
)

func TestRealFilingNewPostHeadBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, boundary, wantError                  string
		head                                       any
		badResponse, readFailure, legacy, wantPost bool
		trunkMerge                                 bool
	}{
		{name: "matching", head: strings.Repeat("a", 40), wantPost: true},
		{name: "trunk merge after wait", boundary: "acquire", trunkMerge: true, wantPost: true},
		{name: "stale after wait", boundary: "acquire", head: strings.Repeat("b", 40), wantError: "後続案とPRのheadが一致しません。"},
		{name: "stale after build", boundary: "build", head: strings.Repeat("b", 40), wantError: "後続案とPRのheadが一致しません。"},
		{name: "missing", boundary: "acquire", wantError: "PRのhead.shaが欠測または不正です。再確認します。"},
		{name: "invalid", boundary: "acquire", head: 42, wantError: "PRのhead.shaが欠測または不正です。再確認します。"},
		{name: "bad response", boundary: "acquire", badResponse: true, wantError: "PRの応答を照合できません。"},
		{name: "read failure", boundary: "acquire", readFailure: true, wantError: "read failed"},
		{name: "legacy ID", boundary: "acquire", legacy: true, wantPost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			ctx := context.Background()
			head := strings.Repeat("a", 40)
			p := v3()
			if !tc.legacy {
				p["id"] = workerFixtureID(head, "1")
			}
			comment := proposalComment(p, 10)
			api.comments = []Object{comment}
			api.pull["head"] = Object{"sha": head}
			parsed, fingerprint, err := parse(str(comment["body"]))
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.adoptionDraft(ctx, "owner/repo", 1, comment, parsed, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			alter := func() {
				if tc.trunkMerge {
					// The draft was made at a, then trunk was merged at b and c.
					trunkMergeFixture(api)
					return
				}
				if tc.readFailure {
					api.err = fmt.Errorf("read failed")
				}
				if tc.badResponse {
					api.pull["number"] = 99
				}
				if tc.head == nil {
					delete(api.pull, "head")
				} else {
					api.pull["head"] = Object{"sha": tc.head}
				}
			}
			queue, err := jobs.New(s.o.AppDB, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			checks, failedChecks, shared := 0, 0, 0
			pulls := 0
			api.before = func(route string) {
				if route == "/repos/owner/repo/pulls/2" {
					pulls++
				}
			}
			filer, err := filing.New(ctx, filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(u Object) bool { return u["login"] == "intake" }, ValidateFollowup: s.ValidateDraft, ValidateNewPost: func(c context.Context, d consultations.Record) error {
				checks++
				before := pulls
				if tc.badResponse || tc.readFailure {
					// Check has already cached a valid response in the production boundary.
					// Bypass that cache only to exercise ValidateNewPost's own error paths;
					// in production Check rejects these errors first.
					alter()
					c = context.Background()
				}
				err := s.ValidateNewPost(c, d)
				if err != nil && err.Error() != tc.wantError {
					t.Errorf("ValidateNewPost returned %v, want %q", err, tc.wantError)
				}
				if pulls == before {
					shared++
				}
				if err != nil {
					failedChecks++
				}
				return err
			},
				Acquire: func(ctx context.Context, _ string) (context.Context, func(), error) {
					if tc.boundary == "acquire" && !tc.readFailure && !tc.badResponse {
						alter()
					}
					return ctx, func() {}, nil
				},
				Check: s.ValidateDraft, Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
					prepared, e := s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
					if tc.boundary == "build" {
						alter()
					}
					return prepared, e
				},
				Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = filer.SubmitAccepted(ctx, str(d["id"]), consultations.Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
			if (api.posts == 1) != tc.wantPost {
				t.Fatalf("posts=%d err=%v", api.posts, err)
			}
			if !tc.wantPost && (err == nil || failedChecks != 1) {
				t.Fatalf("ValidateNewPost failure missing: err=%v calls=%d failures=%d", err, checks, failedChecks)
			}
			if tc.wantPost && (checks != 2 || shared != 2) {
				t.Fatalf("validation did not share both PR reads: checks=%d shared=%d", checks, shared)
			}
			if !tc.wantPost && err.Error() != tc.wantError {
				t.Fatalf("wrong refusal: %v", err)
			}
		})
	}
}

func TestRealFilingReconcilesAfterHeadChanges(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			ctx := context.Background()
			head := strings.Repeat("a", 40)
			p := v3()
			p["id"] = workerFixtureID(head, "2")
			comment := proposalComment(p, 10)
			api.comments = []Object{comment}
			api.pull["head"] = Object{"sha": head}
			parsed, fingerprint, err := parse(str(comment["body"]))
			if err != nil {
				t.Fatal(err)
			}
			d, err := s.adoptionDraft(ctx, "owner/repo", 1, comment, parsed, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := jobs.New(s.o.AppDB, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			checks, builds := 0, 0
			opts := filing.Options{Records: s.records, API: api, Jobs: queue, Project: s.o.Project, Own: func(u Object) bool { return u["login"] == "intake" }, ValidateFollowup: s.ValidateDraft,
				ValidateNewPost: func(ctx context.Context, d consultations.Record) error { checks++; return s.ValidateNewPost(ctx, d) }, Check: s.ValidateDraft,
				Build: func(ctx context.Context, d consultations.Record) (filing.Prepared, error) {
					builds++
					return s.PrepareFollowup(ctx, d, Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
				}, Link: func(repo string, n int64) string { return s.o.Link(repo, "issues", n) }}
			filer, err := filing.New(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			api.unknown = true
			if _, err = filer.SubmitAccepted(ctx, str(d["id"]), consultations.Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen}); err == nil {
				t.Fatal("expected unknown outcome")
			}
			if missing {
				delete(api.pull, "head")
			} else {
				api.pull["head"] = Object{"sha": strings.Repeat("b", 40)}
			}
			filer, err = filing.New(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = filer.SubmitAccepted(ctx, str(d["id"]), nil, auth.Principal{Kind: auth.Screen})
			saved, _, err := s.records.Get(ctx, "drafts", str(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			if api.posts != 1 || checks != 2 || builds != 1 || saved["status"] != "submitted" {
				t.Fatalf("posts=%d checks=%d builds=%d saved=%v", api.posts, checks, builds, saved)
			}
		})
	}
}

func TestValidateNewPostReadsRealHeadAndIsCandidateScoped(t *testing.T) {
	s, api, _ := adoptFixture(t)
	ctx := context.Background()
	head := strings.Repeat("a", 40)
	drafts := make([]consultations.Record, 0, 2)
	for i, batch := range []string{"1", "2"} {
		p := v3()
		p["id"] = workerFixtureID(head, batch)
		comment := proposalComment(p, int64(10+i))
		api.comments = append(api.comments, comment)
		parsed, fingerprint, err := parse(str(comment["body"]))
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.adoptionDraft(ctx, "owner/repo", 1, comment, parsed, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		drafts = append(drafts, d)
	}
	for _, tc := range []struct {
		name    string
		mutate  func()
		wantErr bool
	}{
		{"match", func() { api.pull["head"] = Object{"sha": head} }, false},
		{"stale", func() { api.pull["head"] = Object{"sha": strings.Repeat("b", 40)} }, true},
		{"missing", func() { delete(api.pull, "head") }, true},
		{"malformed", func() { api.pull["head"] = Object{"sha": 7} }, true},
		{"bad response", func() { api.pull["number"] = 3 }, true},
		{"read failure", func() { api.err = fmt.Errorf("read failure") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.err = nil
			api.pull["number"] = 2
			tc.mutate()
			if err := s.ValidateNewPost(ctx, drafts[0]); (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
		})
	}
	api.err = nil
	api.pull["number"] = 2
	api.pull["head"] = Object{"sha": head}
	if err := s.ValidateNewPost(ctx, drafts[1]); err != nil {
		t.Fatal("別batchに拒否が波及しました", err)
	}
}

func TestValidateNewPostTrunkMergeAndLaterProposal(t *testing.T) {
	s, api, _ := adoptFixture(t)
	old, posted, _ := trunkMergeFixture(api)
	p := v3()
	p["id"] = workerFixtureID(posted, "1")
	comment := proposalComment(p, 10)
	oldP := v3()
	oldP["id"] = workerFixtureID(old, "1")
	api.comments = []Object{proposalComment(oldP, 9), comment}
	parsed, fingerprint, err := parse(str(comment["body"]))
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.adoptionDraft(context.Background(), "owner/repo", 1, comment, parsed, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateNewPost(context.Background(), d); err != nil {
		t.Fatalf("trunk merge: %v", err)
	}
	newest := v3()
	newest["id"] = workerFixtureID(strings.Repeat("c", 40), "2")
	api.comments = append(api.comments, proposalComment(newest, 11))
	if err = s.ValidateNewPost(context.Background(), d); err != errStaleWorkerHead {
		t.Fatalf("later posted head accepted: %v", err)
	}
	api.comments = api.comments[:2]
	api.commitErr = fmt.Errorf("unavailable")
	if err = s.ValidateNewPost(context.Background(), d); err == nil || err == errStaleWorkerHead {
		t.Fatalf("read failure must be retryable: %v", err)
	}
	api.commitErr = nil
	api.commits["/repos/owner/repo/pulls/2/commits?limit=50&page=1"][2]["parents"] = []any{Object{"sha": posted}}
	if err = s.ValidateNewPost(context.Background(), d); err != errStaleWorkerHead {
		t.Fatalf("own change accepted: %v", err)
	}
}
