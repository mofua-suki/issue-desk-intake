package filing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

// Exercise the shipped Gitea response decoder and filing service against the
// same repository responses used for posting, history, and archive inspection.
type repositoryResponses struct {
	posts         map[string]int
	issues        map[string][]map[string]any
	reject        map[string]bool
	archive       map[string]string
	lookups       map[string]int
	lookupStatus  int
	historyStatus int
	unknown       bool
	lose          bool
}

func (a *repositoryResponses) RoundTrip(r *http.Request) (*http.Response, error) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("unexpected path")
	}
	repo := strings.ToLower(parts[0] + "/" + parts[1])
	status, body := 200, "{}"
	switch {
	case len(parts) == 2 && r.Method == "GET":
		a.lookups[repo]++
		body = a.archive[repo]
		if body == "" {
			body = `{"archived":false}`
		}
		if a.lookupStatus != 0 {
			status = a.lookupStatus
		}
	case len(parts) == 3 && parts[2] == "issues" && r.Method == "POST":
		a.posts[repo]++
		if a.reject[repo] {
			status = 423
			break
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		issue := map[string]any{"number": a.posts[repo], "title": payload["title"], "body": payload["body"], "state": "open", "user": map[string]any{"id": 10}, "created_at": "2026-11-01T00:00:00Z"}
		if !a.lose {
			a.issues[repo] = append(a.issues[repo], issue)
		}
		data, err := json.Marshal(issue)
		if err != nil {
			return nil, err
		}
		status, body = 201, string(data)
		if a.unknown {
			body = "unreadable-success"
			a.unknown = false
		}
	case len(parts) >= 3 && parts[2] == "issues" && r.Method == "GET":
		if a.historyStatus != 0 {
			status = a.historyStatus
			break
		}
		var value any = a.issues[repo]
		if a.issues[repo] == nil {
			value = []any{}
		}
		if len(parts) == 4 {
			value = nil
			for _, issue := range a.issues[repo] {
				if fmt.Sprint(issue["number"]) == parts[3] {
					value = issue
				}
			}
			if value == nil {
				status = 404
			}
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		body = string(data)
	default:
		return nil, fmt.Errorf("unexpected method/path")
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func archivedFixture(t *testing.T) (*Service, *repositoryResponses) {
	t.Helper()
	s, _ := fixture(t)
	responses := &repositoryResponses{posts: map[string]int{}, issues: map[string][]map[string]any{}, reject: map[string]bool{"owner/app": true}, archive: map[string]string{"owner/app": `{"archived":true}`}, lookups: map[string]int{}}
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("isolated-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.example", token, responses)
	if err != nil {
		t.Fatal(err)
	}
	s.options.API, s.options.RepositoryArchived = client, client.RepositoryArchived
	return s, responses
}

func reopenArchivedService(t *testing.T, s *Service) *Service {
	t.Helper()
	records, err := consultations.OpenRecords(context.Background(), s.options.Records.DB)
	if err != nil {
		t.Fatal(err)
	}
	options := s.options
	options.Records = records
	restored, err := New(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestArchivedRepository423Reason(t *testing.T) {
	s, api := archivedFixture(t)
	ctx := context.Background()
	consumer, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Project: s.options.Project, Submit: s.SubmitExplicit})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	accepted, err := consumer.AcceptSubmission(ctx, draftID, Record{"revision": 0}, p)
	if err != nil || accepted["accepted"] != true {
		t.Fatalf("accept: %v %v", accepted, err)
	}
	d, err := s.SubmitAccepted(ctx, draftID, Record{"revision": accepted["revision"]}, p)
	if err == nil || d["status"] != "submit_error" || d["attempted"] != false {
		t.Fatalf("draft=%v err=%v", d, err)
	}
	saved := getCorrectionRecord(t, reopenArchivedService(t, s), draftID)
	for _, part := range []string{"起票先 owner/app は保管済みで新しいIssueを受け付けません", "同じ内容での再起票では解消しません", "登録済みの別プロジェクト", "終了"} {
		if !strings.Contains(fleet.Text(saved["error"]), part) {
			t.Fatalf("missing guidance %s: %v", part, saved)
		}
	}
	stops := fleet.Items(saved["repositoryStops"])
	if len(stops) != 1 || row(stops[0])["project"] != "owner/app" || row(stops[0])["reason"] != "archived" || fleet.Text(row(stops[0])["stoppedAt"]) == "" || row(stops[0])["message"] != saved["error"] || saved["errorCode"] != "repository_archived" || api.posts["owner/app"] != 1 || api.lookups["owner/app"] != 1 {
		t.Fatalf("saved=%v posts=%v lookups=%v", saved, api.posts, api.lookups)
	}
	if !strings.Contains(fleet.Text(row(saved["lastFailure"])["cause"]), "保管済み") {
		t.Fatal(saved["lastFailure"])
	}
	// The API consumer receives the same reason, code, and persisted evidence.
	response := httptest.NewRecorder()
	problem, ok := err.(*contract.Problem)
	if !ok {
		t.Fatalf("unexpected API error: %v", err)
	}
	contract.WriteProblem(response, problem)
	var envelope Record
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != 409 || envelope["code"] != "repository_archived" || envelope["error"] != saved["error"] {
		t.Fatalf("API response=%s err=%v", response.Body.String(), err)
	}
	read, err := consumer.Draft(ctx, draftID, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	view := consumer.DraftView(read, auth.Principal{Kind: auth.Screen}, "https://desk.example")
	if view["error"] != saved["error"] || encoded(view["repositoryStops"]) != encoded(saved["repositoryStops"]) {
		t.Fatalf("API draft view=%v", view)
	}
}

func TestArchivedRepositoryStopPersistsAcrossEntrypoints(t *testing.T) {
	s, api := archivedFixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "sender", Delegated: true}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, p); err == nil {
		t.Fatal("expected rejection")
	}
	original := getCorrectionRecord(t, s, draftID)
	s = reopenArchivedService(t, s)
	api.reject["owner/app"] = false
	api.archive["owner/app"] = `{"archived":false}`
	s.options.Build = func(context.Context, Record) (Prepared, error) {
		t.Fatal("stopped destination rebuilt")
		return Prepared{}, nil
	}
	for index, submit := range []func(context.Context, string, Record, auth.Principal) (Record, error){s.Submit, s.SubmitExplicit, s.SubmitAccepted} {
		d := changeCorrectionRecord(t, s, draftID, func(d Record) {
			consultations.SetDraftError(d, nil, nil)
			d["status"], d["project"] = "auto_ready", "owner/app"
			if index == 1 {
				d["project"] = "OWNER/APP"
			}
		})
		result, err := submit(ctx, draftID, Record{"revision": d["revision"]}, p)
		if err == nil || result["status"] != "submit_error" || result["attempted"] != false || result["error"] != original["error"] || encoded(result["repositoryStops"]) != encoded(original["repositoryStops"]) || api.posts["owner/app"] != 1 || api.lookups["owner/app"] != 1 {
			t.Fatalf("result=%v err=%v posts=%v", result, err, api.posts)
		}
	}
	// The sender policy still runs before the persisted stop check.
	_, err := s.SubmitAccepted(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.PC, TokenID: "other"})
	if err == nil {
		t.Fatal("unrelated token accepted")
	}
}

func TestRepository423Classification(t *testing.T) {
	for _, tc := range []struct {
		name, archive  string
		status         int
		code, guidance string
	}{
		{"active", `{"archived":false}`, 0, "repository_locked", "一時的なロック"},
		{"missing", `{}`, 0, "repository_archive_unverified", "保管状態を確認できない"},
		{"invalid", `{"archived":"true"}`, 0, "repository_archive_unverified", "保管状態を確認できない"},
		{"null", `{"archived":null}`, 0, "repository_archive_unverified", "保管状態を確認できない"},
		{"lookup-refused", `{}`, 403, "repository_archive_unverified", "保管状態を確認できない"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api := archivedFixture(t)
			api.archive["owner/app"], api.lookupStatus = tc.archive, tc.status
			d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
			if err == nil || d["status"] != "submit_error" || d["attempted"] != false || len(fleet.Items(d["repositoryStops"])) != 0 || d["errorCode"] != tc.code || !strings.Contains(fleet.Text(d["error"]), tc.guidance) {
				t.Fatalf("draft=%v err=%v", d, err)
			}
			s = reopenArchivedService(t, s)
			api.reject["owner/app"] = false
			result, err := s.SubmitExplicit(context.Background(), draftID, Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
			if err != nil || result["status"] != "submitted" || api.posts["owner/app"] != 2 {
				t.Fatalf("draft=%v err=%v posts=%v", result, err, api.posts)
			}
		})
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-missing-%v", missing), func(t *testing.T) {
			s, api := archivedFixture(t)
			api.reject["owner/app"], api.unknown, api.lose = false, true, missing
			d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
			if err == nil || d["status"] != "submit_unknown" || api.lookups["owner/app"] != 0 {
				t.Fatalf("draft=%v err=%v", d, err)
			}
			s = reopenArchivedService(t, s)
			result, err := s.SubmitAccepted(context.Background(), draftID, Record{}, auth.Principal{Kind: auth.Screen})
			expected := "submitted"
			if missing {
				expected = "submit_unknown"
			}
			if (err != nil) != missing || result["status"] != expected || api.posts["owner/app"] != 1 || api.lookups["owner/app"] != 0 || len(fleet.Items(result["repositoryStops"])) != 0 {
				t.Fatalf("draft=%v err=%v posts=%v", result, err, api.posts)
			}
		})
	}
	t.Run("history-423", func(t *testing.T) {
		s, api := archivedFixture(t)
		api.historyStatus = 423
		d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
		if err == nil || d["status"] != "submit_error" || len(fleet.Items(d["repositoryStops"])) != 0 || api.lookups["owner/app"] != 0 || api.posts["owner/app"] != 0 {
			t.Fatalf("draft=%v err=%v", d, err)
		}
	})
	t.Run("attachment-423", func(t *testing.T) {
		s, api := archivedFixture(t)
		api.reject["owner/app"] = false
		changeCorrectionRecord(t, s, draftID, func(d Record) { d["messages"] = []any{Record{"images": []any{Record{"id": "image"}}}} })
		s.options.Attach = func(context.Context, Record, map[string]any) error { return &gitea.Error{Status: 423} }
		d, err := s.Submit(context.Background(), draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
		if err == nil || d["status"] != "submit_unknown" || len(fleet.Items(d["repositoryStops"])) != 0 || api.lookups["owner/app"] != 0 {
			t.Fatalf("draft=%v err=%v", d, err)
		}
		s.options.Attach = func(context.Context, Record, map[string]any) error { return nil }
		result, err := s.SubmitAccepted(context.Background(), draftID, Record{}, auth.Principal{Kind: auth.Screen})
		if err != nil || result["status"] != "submitted" || api.posts["owner/app"] != 1 {
			t.Fatalf("draft=%v err=%v posts=%v", result, err, api.posts)
		}
	})
}

func TestArchivedRepositoryAllowsExistingIssueRecoveryAndOtherDestination(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-%v", existing), func(t *testing.T) {
			s, api := archivedFixture(t)
			ctx := context.Background()
			d, err := s.Submit(ctx, draftID, Record{"revision": 0}, auth.Principal{Kind: auth.Screen})
			if err == nil {
				t.Fatal("expected archive stop")
			}
			savedStops := encoded(d["repositoryStops"])
			s = reopenArchivedService(t, s)
			if existing {
				api.issues["owner/app"] = []map[string]any{{"number": 41, "title": "回収", "body": d["postBody"], "state": "open", "user": map[string]any{"id": 10}}}
				s.options.Build = func(context.Context, Record) (Prepared, error) {
					t.Fatal("recovery rebuilt saved job")
					return Prepared{}, nil
				}
			} else {
				d = changeCorrectionRecord(t, s, draftID, func(d Record) { d["project"] = "owner/other" })
			}
			result, err := s.SubmitAccepted(ctx, draftID, Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
			if err != nil || result["status"] != "submitted" || encoded(result["repositoryStops"]) != savedStops || api.posts["owner/app"] != 1 {
				t.Fatalf("draft=%v err=%v posts=%v", result, err, api.posts)
			}
			issue := row(result["issue"])
			number, _ := integer(issue["number"])
			job, err := s.options.Jobs.ByIssue(ctx, fleet.Text(issue["project"]), number)
			if err != nil || job == nil || len(job.Scope) != 1 || job.Scope[0] != "a.go" {
				t.Fatalf("job=%v err=%v", job, err)
			}
			if !existing && api.posts["owner/other"] != 1 {
				t.Fatal(api.posts)
			}
		})
	}
}

func TestArchivedRepositorySplitRecovery(t *testing.T) {
	s, api := archivedFixture(t)
	ctx := context.Background()
	api.reject["owner/app"] = false
	// Use the shipped planner and body/job builder for the persisted split.
	changeCorrectionRecord(t, s, draftID, func(d Record) {
		d["splitIssues"] = correctionProposals()
		d["body"] = "共通条件: 認証を維持する。未確認事項: 実環境の保管理由。"
		d["messages"] = []any{Record{"id": "source", "role": "user", "text": "共通条件と未確認事項を継承する", "at": "2026-11-01T00:00:00Z"}}
		d["completionPlan"] = planFixture().Record()
	})
	s.options.SplitPlan = PlanSplit
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		config := configFixture()
		config.Records, config.Project = s.options.Records, s.options.Project
		return Prepare(ctx, config, d)
	}
	build := s.options.Build
	s.options.BeforeSplitChild = func(_ context.Context, _ Record, child Record, _ []any) (Record, error) {
		n, _ := integer(child["splitIndex"])
		if n == 2 {
			api.unknown = true
		}
		return child, nil
	}
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.Submit(ctx, draftID, Record{"revision": 0}, p); err == nil {
		t.Fatal("expected unknown child")
	}
	parent := getCorrectionRecord(t, s, draftID)
	ids := fleet.Items(parent["splitChildren"])
	if len(ids) != 3 {
		t.Fatalf("children=%v parent=%v", ids, parent)
	}
	firstJob, err := s.options.Jobs.ByIssue(ctx, "owner/app", 1)
	if err != nil || firstJob == nil {
		t.Fatalf("first job=%v err=%v", firstJob, err)
	}
	operationID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	firstJob, err = s.options.Jobs.Control(ctx, firstJob.ID, jobs.ControlInput{ID: operationID, Action: "stop", By: "fixture", Reason: "利用者が成功子を停止"}, jobs.ControlOptions{})
	if err != nil || firstJob.Status != "stopped" {
		t.Fatalf("job=%v err=%v", firstJob, err)
	}
	firstJobBefore := encoded(firstJob)
	firstBefore := encoded(getCorrectionRecord(t, s, fleet.Text(ids[0])))
	unknown := getCorrectionRecord(t, s, fleet.Text(ids[1]))
	if unknown["status"] != "submit_unknown" || api.posts["owner/app"] != 2 {
		t.Fatalf("unknown=%v posts=%v", unknown, api.posts)
	}
	s = reopenArchivedService(t, s)
	s.options.BeforeSplitChild = nil
	api.reject["owner/app"] = true
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		if d["id"] == ids[1] {
			t.Fatal("unknown child rebuilt")
		}
		return build(ctx, d)
	}
	// Reconcile the unknown child, then persist the archived destination's refusal.
	stopped, err := s.SubmitAccepted(ctx, draftID, Record{"revision": parent["revision"]}, p)
	if err == nil || stopped["status"] != "submit_error" || api.posts["owner/app"] != 3 {
		t.Fatalf("parent=%v err=%v posts=%v", stopped, err, api.posts)
	}
	second := getCorrectionRecord(t, s, fleet.Text(ids[1]))
	if second["status"] != "submitted" || encoded(second["preparedJob"]) != encoded(unknown["preparedJob"]) || encoded(second["dependencies"]) != encoded(unknown["dependencies"]) {
		t.Fatal(second)
	}
	secondBefore := encoded(second)
	archived := getCorrectionRecord(t, s, fleet.Text(ids[2]))
	s = reopenArchivedService(t, s)
	s.options.Build = func(context.Context, Record) (Prepared, error) {
		t.Fatal("recovery rebuilt child")
		return Prepared{}, nil
	}
	api.reject["owner/app"] = false
	result, err := s.SubmitExplicit(ctx, draftID, Record{"revision": stopped["revision"]}, p)
	if err == nil || result["status"] != "submit_error" || encoded(result["splitChildren"]) != encoded(ids) || api.posts["owner/app"] != 3 || api.lookups["owner/app"] != 1 {
		t.Fatalf("parent=%v err=%v posts=%v", result, err, api.posts)
	}
	if encoded(getCorrectionRecord(t, s, fleet.Text(ids[0]))) != firstBefore || encoded(getCorrectionRecord(t, s, fleet.Text(ids[1]))) != secondBefore {
		t.Fatal("successful child progress changed")
	}
	after := getCorrectionRecord(t, s, fleet.Text(ids[2]))
	for _, key := range []string{"repositoryStops", "dependencies", "_dependsOn", "preparedJob", "_splitFingerprint"} {
		if encoded(after[key]) != encoded(archived[key]) {
			t.Fatalf("archived child changed %s", key)
		}
	}
	for i := 0; i < 2; i++ {
		child := getCorrectionRecord(t, s, fleet.Text(ids[i]))
		issue := row(child["issue"])
		number, _ := integer(issue["number"])
		if number != int64(i+1) {
			t.Fatal(issue)
		}
		job, err := s.options.Jobs.ByIssue(ctx, fleet.Text(issue["project"]), number)
		if err != nil || job == nil {
			t.Fatalf("job=%v err=%v", job, err)
		}
		if i == 0 && encoded(job) != firstJobBefore {
			t.Fatalf("successful job progress changed: %v", job)
		}
		if encoded(job.View["completionPlan"]) != encoded(planFixture().Record()) {
			t.Fatal("inherited completion plan lost", job.View)
		}
	}
	if err := s.options.Jobs.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	secondJob, err := s.options.Jobs.ByIssue(ctx, "owner/app", 2)
	if err != nil || secondJob == nil {
		t.Fatalf("second job=%v err=%v", secondJob, err)
	}
	detail, err := s.options.Jobs.Detail(ctx, secondJob.ID)
	if err != nil || encoded(detail["dependsOn"]) != encoded([]int64{firstJob.ID}) {
		t.Fatalf("dependencies=%v err=%v", detail, err)
	}
}
