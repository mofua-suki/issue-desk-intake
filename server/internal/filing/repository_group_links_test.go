package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

type countingLinkRecords struct {
	records map[string]consultations.Record
	reads   map[string]int
}

func (r *countingLinkRecords) Get(_ context.Context, _ string, id string) (consultations.Record, bool, error) {
	r.reads[id]++
	d, ok := r.records[id]
	return d, ok, nil
}

func TestGroupLinkWaitingReadsEachAncestorOnce(t *testing.T) {
	r := &countingLinkRecords{records: map[string]consultations.Record{
		"leaf":   {"parentSplit": "middle"},
		"middle": {"parentSplit": "root"},
		"root":   {"_groupLinkGroups": []any{[]any{"a", "b"}}},
	}, reads: map[string]int{}}
	waiting, err := groupLinkWaiting(context.Background(), r, "leaf")
	if err != nil || !waiting {
		t.Fatalf("waiting=%v err=%v", waiting, err)
	}
	for _, id := range []string{"leaf", "middle", "root"} {
		if r.reads[id] != 1 {
			t.Errorf("%s read %d times", id, r.reads[id])
		}
	}
	r.records["root"]["_groupLinksComplete"] = true
	waiting, err = groupLinkWaiting(context.Background(), r, "leaf")
	if err != nil || waiting {
		t.Fatalf("completed: waiting=%v err=%v", waiting, err)
	}
	delete(r.records, "root")
	if _, err = groupLinkWaiting(context.Background(), r, "leaf"); err == nil {
		t.Fatal("missing parent accepted")
	}
	r.records["middle"]["parentSplit"] = "leaf"
	if _, err = groupLinkWaiting(context.Background(), r, "leaf"); err == nil {
		t.Fatal("cycle accepted")
	}
}

type linkFixture struct {
	*memoryAPI
	failUpdate    bool
	conflict      bool
	failPost      bool
	unknownUpdate bool
}

func (a *linkFixture) CreateIssue(ctx context.Context, repo string, payload any) (map[string]any, error) {
	if a.failPost && a.posts == 1 {
		a.failPost = false
		return nil, &gitea.Error{Status: 400}
	}
	return a.memoryAPI.CreateIssue(ctx, repo, payload)
}
func (a *linkFixture) Get(ctx context.Context, repo string, n int64) (map[string]any, error) {
	issue, err := a.memoryAPI.Issue(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	copy := map[string]any{}
	for k, v := range issue {
		copy[k] = v
	}
	copy["content_version"] = 1
	return copy, nil
}
func (a *linkFixture) Update(ctx context.Context, repo string, n int64, body string, version any) error {
	if a.failUpdate {
		a.failUpdate = false
		return errors.New("communication failed")
	}
	if a.conflict {
		a.conflict = false
		return &gitea.Error{Status: 409}
	}
	issue, err := a.memoryAPI.Issue(ctx, repo, n)
	if err != nil {
		return err
	}
	issue["body"] = body
	if a.unknownUpdate {
		a.unknownUpdate = false
		return &gitea.Error{UnknownOutcome: true}
	}
	return nil
}
func setupLinks(t *testing.T) (*Service, *linkFixture) {
	t.Helper()
	s, api := fixture(t)
	f := &linkFixture{memoryAPI: api}
	s.options.API = f
	s.options.GroupIssueLinks = f
	s.options.RepositoryGroups = func(context.Context) ([][]string, error) { return [][]string{{"owner/a", "owner/b"}}, nil }
	s.options.SplitPlan = func(Record) ([]Record, error) {
		return []Record{{"project": "owner/a", "title": "A", "dependencies": []any{42}, "messages": []any{Record{"id": "source-1", "text": "依頼者の発言"}}}, {"project": "owner/b", "title": "B", "messages": []any{Record{"id": "source-1", "text": "依頼者の発言"}}}}, nil
	}
	s.options.Build = func(_ context.Context, d Record) (Prepared, error) {
		body := "本文\n依存: #42\n<!-- issue-desk:" + fleet.Text(d["id"]) + " -->"
		return Prepared{Title: fleet.Text(d["title"]), Body: body, Job: jobs.NewJob{Scope: []string{"a.go"}, View: map[string]any{"completionPlanEvidence": Record{"bodySHA256": "old"}, "completionApproval": Record{"source": "user"}}}}, nil
	}
	changeCorrectionRecord(t, s, draftID, func(d Record) { d["splitIssues"] = []any{Record{}, Record{}} })
	return s, f
}
func submitLinks(s *Service) (Record, error) {
	d, _, _ := s.options.Records.Get(context.Background(), "drafts", draftID)
	return s.Submit(context.Background(), draftID, Record{"revision": d["revision"]}, auth.Principal{Kind: auth.Screen})
}
func TestRepositoryGroupIssueLinks(t *testing.T) {
	s, a := setupLinks(t)
	result, err := submitLinks(s)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "submitted" || a.posts != 2 {
		t.Fatal(result, a.posts)
	}
	if !strings.Contains(fleet.Text(a.issues[0]["body"]), "owner/b#2") || !strings.Contains(fleet.Text(a.issues[1]["body"]), "owner/a#1") || !strings.Contains(fleet.Text(a.issues[0]["body"]), "本文") || !strings.Contains(fleet.Text(a.issues[0]["body"]), "依存: #42") {
		t.Fatal(a.issues)
	}
	child := getCorrectionRecord(t, s, fleet.Text(fleet.Items(result["splitChildren"])[0]))
	if encoded(child["dependencies"]) != "[42]" {
		t.Fatal(child)
	}
}

func TestSubmittedSplitReturnsSavedIssuesAfterRestart(t *testing.T) {
	s, api := setupLinks(t)
	result, err := submitLinks(s)
	if err != nil || result["status"] != "submitted" || len(fleet.Items(result["issues"])) != 2 || api.posts != 2 {
		t.Fatalf("initial submission: result=%v posts=%d err=%v", result, api.posts, err)
	}
	before := getCorrectionRecord(t, s, draftID)
	restarted, err := New(context.Background(), s.options)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := submitLinks(restarted)
	if err != nil || !reflect.DeepEqual(resumed["issues"], before["issues"]) || api.posts != 2 {
		t.Fatalf("resume: result=%v posts=%d err=%v", resumed, api.posts, err)
	}
	after := getCorrectionRecord(t, restarted, draftID)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(resumed, before) {
		t.Fatalf("parent changed: before=%v after=%v resumed=%v", before, after, resumed)
	}
}

func TestRepositoryGroupLinksPendingFailureKeepsParentSubmitted(t *testing.T) {
	s, a := setupLinks(t)
	ctx := context.Background()
	failDelivery := true
	s.options.Project = func(repo string) error {
		parent := getCorrectionRecord(t, s, draftID)
		if failDelivery && parent["status"] == "submitted" {
			return errors.New("delivery unavailable")
		}
		return nil
	}
	result, err := submitLinks(s)
	if err != nil || result["status"] != "submitted" || a.posts != 2 {
		t.Fatal(result, err, a.posts)
	}
	parent := getCorrectionRecord(t, s, draftID)
	if parent["status"] != "submitted" || !strings.Contains(fleet.Text(a.issues[0]["body"]), "owner/b#2") || !strings.Contains(fleet.Text(a.issues[1]["body"]), "owner/a#1") {
		t.Fatal(parent, a.issues)
	}
	if job, err := s.options.Jobs.ByIssue(ctx, "owner/a", 1); err != nil || job != nil {
		t.Fatal(job, err)
	}
	failDelivery = false
	if err := s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	for i, repo := range []string{"owner/a", "owner/b"} {
		if job, err := s.options.Jobs.ByIssue(ctx, repo, int64(i+1)); err != nil || job == nil {
			t.Fatal(job, err)
		}
	}
	if parent = getCorrectionRecord(t, s, draftID); parent["status"] != "submitted" {
		t.Fatal(parent)
	}
}
func TestRepositoryGroupIssueLinksRecovery(t *testing.T) {
	s, a := setupLinks(t)
	a.failPost = true
	if _, err := submitLinks(s); err == nil {
		t.Fatal("expected failure")
	}
	d := getCorrectionRecord(t, s, draftID)
	if d["status"] == "submitted" {
		t.Fatal(d)
	}
	a.failUpdate = true
	if _, err := submitLinks(s); err == nil {
		t.Fatal("expected update failure")
	}
	d = getCorrectionRecord(t, s, draftID)
	if d["status"] == "submitted" || a.posts != 2 {
		t.Fatal(d, a.posts)
	}
	for i, repo := range []string{"owner/a", "owner/b"} {
		job, err := s.options.Jobs.ByIssue(context.Background(), repo, int64(i+1))
		if err != nil || job != nil {
			t.Fatalf("job released before links: %v %v", job, err)
		}
	}
	a.unknownUpdate = true
	if _, err := submitLinks(s); err == nil {
		t.Fatal("unknown update accepted")
	}
	if getCorrectionRecord(t, s, draftID)["status"] == "submitted" {
		t.Fatal("unknown update completed parent")
	}
	restarted, err := New(context.Background(), s.options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := submitLinks(restarted)
	if err != nil || result["status"] != "submitted" || a.posts != 2 {
		t.Fatal(result, err, a.posts)
	}
	for _, issue := range a.issues {
		if strings.Count(fleet.Text(issue["body"]), "## 同じ組のIssue") != 1 {
			t.Fatal(issue)
		}
	}
}
func TestRepositoryGroupIssueLinksConflict(t *testing.T) {
	s, a := setupLinks(t)
	a.conflict = true
	if _, err := submitLinks(s); err == nil {
		t.Fatal("expected conflict")
	}
	d := getCorrectionRecord(t, s, draftID)
	if d["status"] == "submitted" || strings.Contains(fleet.Text(a.issues[0]["body"]), "同じ組") {
		t.Fatal(d, a.issues)
	}
	child := getCorrectionRecord(t, s, fleet.Text(fleet.Items(d["splitChildren"])[0]))
	if row(child["_groupLinkIntent"]) == nil {
		t.Fatal(child)
	}
}
func TestRepositoryGroupIssueLinksEvidence(t *testing.T) {
	s, a := setupLinks(t)
	result, err := submitLinks(s)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range fleet.Items(result["splitChildren"]) {
		child := getCorrectionRecord(t, s, fleet.Text(id))
		job, err := s.options.Jobs.ByIssue(context.Background(), fleet.Text(child["project"]), int64(i+1))
		if err != nil || job == nil {
			t.Fatal(job, err)
		}
		body := fleet.Text(a.issues[i]["body"])
		savedView := row(row(child["preparedJob"])["View"])
		sum := sha256.Sum256([]byte(body))
		if fleet.Text(row(job.View["completionPlanEvidence"])["bodySHA256"]) != hex.EncodeToString(sum[:]) || fleet.Text(row(job.View["acceptanceTransferInput"])["body"]) != body || fleet.Text(row(job.View["completionApproval"])["source"]) != "user" || fleet.Text(child["postBody"]) != body || fleet.Text(row(savedView["acceptanceTransferInput"])["body"]) != body || fleet.Text(row(fleet.Items(child["messages"])[0])["id"]) != "source-1" {
			t.Fatal(job.View, child)
		}
	}
}

func TestRepositoryGroupLinksNestedWaiting(t *testing.T) {
	s, a := setupLinks(t)
	a.failUpdate = true
	if _, err := submitLinks(s); err == nil {
		t.Fatal("相互参照の更新失敗を受理")
	}
	root := getCorrectionRecord(t, s, draftID)
	childID := fleet.Text(fleet.Items(root["splitChildren"])[0])
	leafID := "55555555-5555-4555-8555-555555555555"
	saveNestedRecord(t, s, Record{"id": leafID, "parentSplit": childID})
	ctx := context.Background()
	var raw string
	if err := s.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM job_filings WHERE key=?"), "owner/a#1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var entry pending
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatal(err)
	}
	entry.DraftID = leafID
	updated, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.options.Records.DB.ExecContext(ctx, bindSQL("UPDATE job_filings SET json=? WHERE key=?"), string(updated), "owner/a#1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := s.options.Jobs.ByIssue(ctx, "owner/a", 1); err != nil || job != nil {
		t.Fatal(job, err)
	}
	changeCorrectionRecord(t, s, childID, func(d Record) { d["parentSplit"] = "99999999-9999-4999-8999-999999999999" })
	if err := s.FilePending(ctx); err == nil {
		t.Fatal("分割元が欠落している配送を受理")
	}
	if job, err := s.options.Jobs.ByIssue(ctx, "owner/a", 1); err != nil || job != nil {
		t.Fatal(job, err)
	}
	changeCorrectionRecord(t, s, childID, func(d Record) { d["parentSplit"] = draftID })
	if _, err := submitLinks(s); err != nil {
		t.Fatal(err)
	}
	if err := s.FilePending(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := s.options.Jobs.ByIssue(ctx, "owner/a", 1); err != nil || job == nil {
		t.Fatal(job, err)
	}
}
