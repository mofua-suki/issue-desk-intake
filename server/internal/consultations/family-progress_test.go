package consultations

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func TestFamilyProgressStatesAndBatch(t *testing.T) {
	calls := 0
	jobs := []FamilyJob{{Project: "a/b", Number: 1, Status: "merged"}, {Project: "a/b", Number: 2, Status: "closed"}, {Project: "a/b", Number: 3, Status: "failed", Reason: "失敗"}, {Project: "a/b", Number: 4, Status: "held", Reason: "保留"}, {Project: "a/b", Number: 5, Status: "stopped"}, {Project: "a/b", Number: 6, Status: "queued", Waiting: []string{"a/b#3"}}}
	s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) { calls++; return jobs, nil }}}
	p := Record{"id": "p", "status": "submitted", "splitChildren": []any{"1", "2", "3", "4", "5", "6", "7", "missing"}, "splitIssues": make([]any, 8)}
	records := []Record{p, {"id": "p2", "splitChildren": []any{}, "splitIssues": []any{}}, {"id": "7", "parentSplit": "p", "status": "submitted", "finishedWithoutIssue": true}}
	for i := 1; i <= 6; i++ {
		records = append(records, Record{"id": string(rune('0' + i)), "parentSplit": "p", "project": "a/b", "status": "submitted", "issue": Record{"number": i}})
	}
	out := s.FamilyProgress(context.Background(), records)
	progress := record(out[0]["familyProgress"])
	if calls != 1 || progress["total"] != 8 || progress["completed"] != 1 || progress["closed"] != 2 || progress["remaining"] != 4 || progress["unknown"] != 1 {
		t.Fatalf("calls=%d progress=%v", calls, progress)
	}
	stalled := fleet.Items(progress["stopped"])
	if len(stalled) != 4 {
		t.Fatalf("stopped=%v", stalled)
	}
	for _, v := range stalled {
		if text(record(v)["reason"]) == "" {
			t.Fatalf("missing reason: %v", v)
		}
	}
	if p["familyProgress"] != nil {
		t.Fatal("stored input mutated")
	}
	jobs[2].Status = "merged"
	jobs[3].Status = "closed"
	jobs[4].Status = "merged"
	jobs[5].Status = "merged"
	p["splitIssues"] = make([]any, 7)
	out = s.FamilyProgress(context.Background(), records)
	if record(out[0]["familyProgress"])["remaining"] != 0 {
		t.Fatalf("not complete: %v", out[0])
	}
}
func TestFamilyProgressOmittedOnReadError(t *testing.T) {
	s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) { return nil, errors.New("unavailable") }}}
	out := s.FamilyProgress(context.Background(), []Record{{"splitChildren": []any{}, "splitIssues": []any{}}})
	if out[0]["familyProgress"] != nil {
		t.Fatal(out)
	}
}

func TestFamilyProgressReadsFinishedSplitFromRecords(t *testing.T) {
	s, _, _ := fixture(t)
	seedFamily(t, s, true)
	ctx := context.Background()
	if _, err := s.Finish(ctx, sendID, Record{}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 5, Status: "stopped"}}, nil
	}
	records, err := s.options.Records.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	out := s.FamilyProgress(ctx, records)
	for _, p := range out {
		if p["id"] == todoID {
			progress := record(s.DraftView(p, auth.Principal{Kind: auth.Screen}, "")["familyProgress"])
			if progress["remaining"] != 1 || progress["closed"] != 1 || len(fleet.Items(progress["stopped"])) != 1 {
				t.Fatal(progress)
			}
			stored, _, e := s.options.Records.Get(ctx, "drafts", todoID)
			if e != nil || stored["familyProgress"] != nil || stored["status"] != "submitted" {
				t.Fatalf("stored=%v err=%v", stored, e)
			}
			return
		}
	}
	t.Fatal("parent missing")
}

func TestClosedChildOverridesStoppedJob(t *testing.T) {
	s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/repo", Number: 1, Status: "stopped"}}, nil
	}}}
	records := []Record{{"id": "p", "splitChildren": []any{"c"}, "splitIssues": []any{Record{}}}, {"id": "c", "parentSplit": "p", "status": "closed", "project": "owner/repo", "issue": Record{"number": 1}}}
	p := record(s.FamilyProgress(context.Background(), records)[0]["familyProgress"])
	if p["closed"] != 1 || p["remaining"] != 0 || len(fleet.Items(p["stopped"])) != 0 {
		t.Fatal(p)
	}
}

func TestFamilyProgressLegacyIssuesAndUnknown(t *testing.T) {
	calls := 0
	s := &Service{options: Options{
		FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
			return []FamilyJob{{Project: "mofua-suki/issue-desk", Number: 968, Status: "running"}, {Project: "mofua-suki/issue-desk", Number: 969, Status: "stopped", Reason: "停止"}, {Project: "mofua-suki/issue-desk", Number: 970, Status: "merged"}}, nil
		},
		FamilyIssues: func(_ context.Context, repo string) (map[string]string, error) {
			calls++
			return map[string]string{"mofua-suki/issue-desk#965": "closed", "mofua-suki/issue-desk#966": "closed", "mofua-suki/issue-desk#967": "closed"}, nil
		},
	}}
	rows := []Record{{"id": "p", "status": "submitted", "splitChildren": []any{"a", "b", "c", "d", "e", "f", "g", "h", "missing"}, "splitIssues": make([]any, 9)}}
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		rows = append(rows, Record{"id": id, "parentSplit": "p", "status": "submitted", "project": "mofua-suki/issue-desk", "issue": Record{"number": 965 + i}})
	}
	rows = append(rows, Record{"id": "g", "parentSplit": "p", "status": "submitted", "finishedWithoutIssue": true}, Record{"id": "h", "parentSplit": "p", "status": "submitted"})
	got := record(s.FamilyProgress(context.Background(), rows)[0]["familyProgress"])
	if calls != 1 || got["total"] != 9 || got["completed"] != 1 || got["closed"] != 4 || got["remaining"] != 2 || got["unknown"] != 2 || len(fleet.Items(got["stopped"])) != 1 {
		t.Fatalf("calls=%d progress=%v", calls, got)
	}
	if rows[0]["familyProgress"] != nil || rows[0]["status"] != "submitted" {
		t.Fatal(rows[0])
	}
}

func TestFamilyProgressIssueReadFailureIsUnknown(t *testing.T) {
	s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) { return nil, nil }, FamilyIssues: func(context.Context, string) (map[string]string, error) { return nil, errors.New("404") }}}
	rows := []Record{{"id": "p", "splitChildren": []any{"c"}, "splitIssues": []any{Record{}}}, {"id": "c", "parentSplit": "p", "status": "submitted", "project": "a/b", "issue": Record{"number": 1}}}
	got := record(s.FamilyProgress(context.Background(), rows)[0]["familyProgress"])
	if got["unknown"] != 1 || got["remaining"] != 0 || got["closed"] != 0 {
		t.Fatal(got)
	}
}

func TestFamilyProgressSingleChildCases(t *testing.T) {
	cases := []struct {
		name, status                                   string
		finished                                       bool
		job                                            *FamilyJob
		issueState                                     string
		completed, closed, remaining, unknown, stopped int
	}{
		{name: "merged", status: "submitted", job: &FamilyJob{Status: "merged"}, completed: 1},
		{name: "closed issue without job", status: "submitted", issueState: "closed", closed: 1},
		{name: "open issue without job", status: "submitted", issueState: "open", remaining: 1},
		{name: "finished without filing", status: "submitted", finished: true, closed: 1},
		{name: "submitted without evidence", status: "submitted", unknown: 1},
		{name: "queued", status: "submitted", job: &FamilyJob{Status: "queued"}, remaining: 1},
		{name: "running", status: "submitted", job: &FamilyJob{Status: "running"}, remaining: 1},
		{name: "dependency", status: "submitted", job: &FamilyJob{Status: "queued", Waiting: []string{"a/b#2"}}, remaining: 1, stopped: 1},
		{name: "failed", status: "submitted", job: &FamilyJob{Status: "failed"}, remaining: 1, stopped: 1},
		{name: "held", status: "submitted", job: &FamilyJob{Status: "held"}, remaining: 1, stopped: 1},
		{name: "stopped", status: "submitted", job: &FamilyJob{Status: "stopped"}, remaining: 1, stopped: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := []FamilyJob{}
			if tc.job != nil {
				j := *tc.job
				j.Project = "a/b"
				j.Number = 1
				jobs = append(jobs, j)
			}
			s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) { return jobs, nil }, FamilyIssues: func(context.Context, string) (map[string]string, error) {
				return map[string]string{"a/b#1": tc.issueState}, nil
			}}}
			c := Record{"id": "c", "parentSplit": "p", "status": tc.status, "project": "a/b", "finishedWithoutIssue": tc.finished}
			if !tc.finished && tc.name != "submitted without evidence" {
				c["issue"] = Record{"number": 1}
			}
			out := s.FamilyProgress(context.Background(), []Record{{"id": "p", "splitChildren": []any{"c"}, "splitIssues": []any{Record{}}}, c})
			p := record(out[0]["familyProgress"])
			if p["completed"] != tc.completed || p["closed"] != tc.closed || p["remaining"] != tc.remaining || p["unknown"] != tc.unknown || len(fleet.Items(p["stopped"])) != tc.stopped {
				t.Fatal(p)
			}
		})
	}
}

func TestFamilyProgressCollectsChildrenWithoutChangingKey(t *testing.T) {
	calls := 0
	var wanted []FamilyIssue
	s := &Service{options: Options{FamilyJobs: func(_ context.Context, issues []FamilyIssue) ([]FamilyJob, error) {
		calls++
		wanted = issues
		return []FamilyJob{{Project: "owner/repo", Number: 1, Status: "merged"}, {Project: "owner/repo", Number: 2, Status: "merged"}}, nil
	}}}
	s.FamilyProgress(context.Background(), []Record{{"id": "child"}})
	if calls != 0 {
		t.Fatal("read without parent")
	}
	rows := []Record{{"id": "p", "splitChildren": []any{"a", "b", "missing"}, "splitIssues": []any{}}, {"id": "a", "parentSplit": "p", "project": "wrong/repo", "issue": Record{"project": "OWNER/REPO", "number": "01"}}, {"id": "b", "parentSplit": "p", "project": "owner/repo", "issue": Record{"number": json.Number("2")}}}
	got := record(s.FamilyProgress(context.Background(), rows)[0]["familyProgress"])
	if !reflect.DeepEqual(wanted, []FamilyIssue{{"OWNER/REPO", 1}, {"owner/repo", 2}}) || got["completed"] != 1 || got["unknown"] != 2 {
		t.Fatalf("wanted=%v progress=%v", wanted, got)
	}
}

func TestMergedChildWaitsForRemainingEvidence(t *testing.T) {
	job := FamilyJob{Project: "owner/repo", Number: 1, Status: "merged", PendingChecks: []string{"コントローラーが稼働版を確認"}}
	s := &Service{options: Options{FamilyJobs: func(context.Context, []FamilyIssue) ([]FamilyJob, error) { return []FamilyJob{job}, nil }}}
	records := []Record{{"id": "p", "splitChildren": []any{"c"}, "splitIssues": []any{Record{}}}, {"id": "c", "parentSplit": "p", "status": "submitted", "project": "owner/repo", "issue": Record{"number": 1}}}
	progress := record(s.FamilyProgress(context.Background(), records)[0]["familyProgress"])
	if progress["completed"] != 0 || progress["remaining"] != 1 || text(record(fleet.Items(progress["stopped"])[0])["status"]) != "verification_waiting" {
		t.Fatal(progress)
	}
	job.PendingChecks = nil
	progress = record(s.FamilyProgress(context.Background(), records)[0]["familyProgress"])
	if progress["completed"] != 1 || progress["remaining"] != 0 {
		t.Fatal(progress)
	}
}
