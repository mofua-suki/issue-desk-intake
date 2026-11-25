package consultationai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

func TestHandoffToPreparedIssue(t *testing.T) {
	const source = "zero\none\ntwo\nthree\nfour\n"
	s, _ := blobFixture(t, func(*http.Request) (string, string) { return "base64", encoded(source) })
	rev := strings.Repeat("a", 40)
	p := &projectState{Name: "owner/other", Branch: "main", Revision: rev, entries: map[string]treeEntry{"docs/a.md": {Path: "docs/a.md", Size: len(source), SHA: fixtureSHA(source)}}}
	d := Record{"_ephemeral": true, "id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": "codex", "title": "Test", "body": "資料 owner/other@" + rev + ":docs/a.md:2-3,5", "ready": true, "questions": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}}
	m := &materials{service: s, draft: d, allowed: map[string]bool{"owner/other": true}, state: contextState{Projects: map[string]*projectState{"owner/other": p}}}
	answer := Record{"ready": true, "title": "Test", "body": d["body"], "questions": []any{}}
	fresh, problem := m.prepareHandoff(context.Background(), d, answer)
	if !fresh || problem != "" {
		t.Fatal(fresh, problem)
	}
	_, shown, _ := m.data(false)
	resumed := resumedPrompt("", d, m, &sessionState{ID: "session"}, shown, turnPromptState{})
	if !strings.Contains(resumed, "2: one") || !strings.Contains(resumed, "5: four") {
		t.Fatalf("resumed consultation omitted handoff: %s", resumed)
	}
	if err := m.rememberDelivered(context.Background(), shown); err != nil {
		t.Fatal(err)
	}
	if fresh, problem = m.prepareHandoff(context.Background(), d, answer); fresh || problem != "" {
		t.Fatal(fresh, problem)
	}
	planAnswer := gateReply()
	planAnswer["project"] = "owner/repo"
	requirement := fleet.Object(fleet.Elements(fleet.Object(planAnswer["executionPlan"])["requirements"])[0])
	requirement["inputs"] = []any{"owner/other@" + rev + ":docs/a.md:2-3,5"}
	if problem := executionPlanWithMaterialsProblem(planAnswer, m); problem != "" {
		t.Fatal(problem)
	}
	if err := s.finalize(context.Background(), d, answer, m); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	var saved filing.Record
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	cfg := filing.BuildConfig{Config: map[string]any{"model": "test"}, Project: func(string) error { return nil }}
	prepared, err := filing.Prepare(context.Background(), cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{rev, "owner/other", "docs/a.md", "2: one", "3: two", "5: four"} {
		if !strings.Contains(prepared.Body, want) {
			t.Fatalf("missing %q: %s", want, prepared.Body)
		}
	}
	if strings.Contains(prepared.Body, "4: three") {
		t.Fatal("unrequested line delivered")
	}
}

func TestHandoffReadFailuresStopBeforeFiling(t *testing.T) {
	for _, tc := range []struct{ name, repo, file, body, reference, reason string }{
		{"missing", "owner/other", "docs/a.md", "one\ntwo", "owner/other:docs/missing.md:1", "ありません"},
		{"denied", "owner/other", "docs/a.md", "one\ntwo", "owner/other:private/secret.txt:1", "許可"},
		{"range", "owner/other", "docs/a.md", "one\ntwo", "owner/other:docs/a.md:1-3", "範囲外"},
		{"limit", "owner/other", "docs/a.md", strings.Repeat("x", handoffLimit+1), "owner/other:docs/a.md:1", "容量上限"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := blobFixture(t, func(*http.Request) (string, string) { return "base64", encoded(tc.body) })
			p := &projectState{Name: tc.repo, Branch: "main", Revision: strings.Repeat("a", 40), entries: map[string]treeEntry{tc.file: {Path: tc.file, Size: len(tc.body), SHA: fixtureSHA(tc.body)}}}
			d := Record{"_ephemeral": true, "id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": "codex", "title": "Test", "body": tc.reference, "ready": true, "questions": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}}
			m := &materials{service: s, draft: d, allowed: map[string]bool{tc.repo: true}, state: contextState{Projects: map[string]*projectState{tc.repo: p}}}
			fresh, problem := m.prepareHandoff(context.Background(), d, Record{"ready": true})
			if fresh || !strings.Contains(problem, tc.reference) || !strings.Contains(problem, tc.reason) || len(m.handoff) != 0 {
				t.Fatal(fresh, problem, m.handoff)
			}
			raw, _ := json.Marshal(d)
			var saved filing.Record
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			cfg := filing.BuildConfig{Config: map[string]any{"model": "test"}, Project: func(string) error { return nil }}
			if _, err := filing.Prepare(context.Background(), cfg, saved); err == nil || !strings.Contains(err.Error(), tc.reference) {
				t.Fatalf("unavailable material was accepted: %v", err)
			}
		})
	}
}

func TestHandoffReadsRequestedHistoricalRevision(t *testing.T) {
	const body = "old one\nold two\n"
	old, current := strings.Repeat("b", 40), strings.Repeat("a", 40)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/git/commits/"):
			json.NewEncoder(w).Encode(Record{"sha": old})
		case strings.Contains(r.URL.Path, "/git/trees/"+old):
			json.NewEncoder(w).Encode(Record{"tree": []any{Record{"path": "docs/a.md", "type": "blob", "mode": "100644", "sha": fixtureSHA(body), "size": len(body)}}, "truncated": false})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			json.NewEncoder(w).Encode(Record{"encoding": "base64", "content": encoded(body)})
		default:
			http.Error(w, "unexpected revision", 404)
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{Gitea: client}}
	p := &projectState{Name: "owner/other", Branch: "main", Revision: current, entries: map[string]treeEntry{}}
	m := &materials{service: s, draft: Record{"project": "owner/repo", "_ephemeral": true}, allowed: map[string]bool{"owner/other": true}, state: contextState{Projects: map[string]*projectState{"owner/other": p}}}
	doc, reason := m.fetchHandoff(context.Background(), "owner/other", old[:8], "docs/a.md", 1, 2)
	if reason != "" || doc.Version != old || doc.Text != "1: old one\n2: old two" || p.Revision != current {
		t.Fatal(doc, reason)
	}
}

func TestSeedIncludesNamedCrossRepositoryRange(t *testing.T) {
	const body = "one\ntwo\nthree\n"
	s, _ := blobFixture(t, func(*http.Request) (string, string) { return "base64", encoded(body) })
	rev := strings.Repeat("a", 40)
	entry := treeEntry{Path: "docs/a.md", Size: len(body), SHA: fixtureSHA(body)}
	d := Record{"_ephemeral": true, "project": "owner/app", "messages": []any{Record{"role": "user", "text": "資料 owner/other:docs/a.md:2-3"}}}
	m := &materials{service: s, draft: d, allowed: map[string]bool{"owner/app": true, "owner/other": true}, state: contextState{Projects: map[string]*projectState{
		"owner/app":   {Name: "owner/app", Branch: "main", Revision: rev, entries: map[string]treeEntry{"docs/a.md": entry}},
		"owner/other": {Name: "owner/other", Branch: "main", Revision: rev, entries: map[string]treeEntry{"docs/a.md": entry}},
	}}}
	if err := m.seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _, _ := m.data(false)
	raw, _ := json.Marshal(data)
	if !strings.Contains(string(raw), "2: two") || !strings.Contains(string(raw), "3: three") || !strings.Contains(string(raw), "owner/other") {
		t.Fatal(string(raw))
	}
}
