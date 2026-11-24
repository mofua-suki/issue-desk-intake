package consultationai

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

const evidenceRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func evidenceFixture(t *testing.T) (*materials, *projectState) {
	t.Helper()
	files := map[string]string{"src/example.go": "first\nsecond\nthird\n", "AGENTS.md": "instructions\n", "README.md": "intro\n"}
	contents := map[string]string{}
	entries := map[string]treeEntry{}
	for name, body := range files {
		sha := fixtureSHA(body)
		contents[sha] = body
		entries[name] = treeEntry{Path: name, SHA: sha, Size: len(body)}
	}
	s, _ := blobFixture(t, func(r *http.Request) (string, string) {
		return "base64", encoded(contents[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	p := &projectState{Name: "owner/app", Branch: "trunk", Revision: evidenceRevision, entries: entries}
	d := Record{"project": "owner/app", "_ephemeral": true, "body": "src/example.go", "readEvidence": Record{"version": 1, "documents": []any{Record{"id": "d1", "project": "owner/app", "path": "src/example.go", "revision": evidenceRevision, "start": 2, "end": 2, "text": "second"}}, "facts": []any{Record{"statement": "reported fact", "documents": []any{"d1"}}}}}
	m := &materials{service: s, draft: d, allowed: map[string]bool{"owner/app": true}, state: contextState{Projects: map[string]*projectState{"owner/app": p}}}
	return m, p
}

func TestRestoreLegacyAliasesKeepsDeliveredRequest(t *testing.T) {
	for _, names := range [][]string{{"README.md", "owner/app:README.md"}, {"owner/app:README.md", "README.md"}} {
		t.Run(names[0], func(t *testing.T) {
			m, p := evidenceFixture(t)
			first, _, err := m.read(context.Background(), ReadRequest{File: names[0], Mode: "full", Context: 5})
			if err != nil {
				t.Fatal(err)
			}
			second := first
			second.Request.File = names[1]
			p.Reads = []Document{first, second}
			key := documentKey(second)
			m.state.Delivered = map[string]bool{key: true}
			m.state.InitialDocuments = map[string]bool{key: true}
			if err := m.restoreReads(context.Background(), p.Name, p); err != nil {
				t.Fatal(err)
			}
			if len(p.Reads) != 2 || documentKey(p.Reads[1]) != key || p.Reads[1].Text != first.Text {
				t.Fatalf("restored reads: %+v", p.Reads)
			}
			// Keep the restored aliases out of the compacted prompt so delivery
			// must come from the persisted or session history, never current input.
			if _, _, err := m.read(context.Background(), ReadRequest{File: "AGENTS.md", Mode: "full", Context: 5}); err != nil {
				t.Fatal(err)
			}
			for _, history := range []string{"saved", "session"} {
				t.Run(history, func(t *testing.T) {
					m.state.Delivered = map[string]bool{}
					session := &sessionState{ID: "session", Sent: map[string]bool{}}
					if history == "saved" {
						m.state.Delivered[key] = true
					} else {
						session.Sent[key] = true
					}
					_, shown, bodies := m.data(true)
					if shown[key] || bodies[p.Name+":README.md"] {
						t.Fatal("alias body remained in compacted input")
					}
					m.addDeliveredBodies(session, bodies)
					if !bodies[p.Name+":README.md"] || !m.state.InitialDocuments[key] {
						t.Fatalf("lost delivery: bodies=%v initial=%v", bodies, m.state.InitialDocuments)
					}
				})
			}
		})
	}
}

func TestReadEvidenceTurnInput(t *testing.T) {
	var inputs []string
	s, d := testServiceSource(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		inputs = append(inputs, r.Input)
		return measuredAnswer(reply()), nil
	}, "first\nsecond\nthird\n")
	evidence := Record{"version": 1, "documents": []any{Record{"id": "d1", "project": "owner/app", "path": "README.md", "revision": evidenceRevision, "start": 2, "end": 2, "text": "second"}}, "facts": []any{Record{"statement": "reported fact", "documents": []any{"d1"}}}}
	if err := s.save(context.Background(), d, Record{"readEvidence": evidence}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 || !strings.Contains(inputs[0], "2: second") || !strings.Contains(inputs[0], "reported fact") || !strings.Contains(inputs[0], evidenceRevision) {
		t.Fatalf("turn input missing evidence: %v", inputs)
	}
}

func TestReadEvidenceConsultationInput(t *testing.T) {
	m, p := evidenceFixture(t)
	ctx := context.Background()
	if err := m.bootstrap(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.seed(ctx); err != nil {
		t.Fatal(err)
	}
	data, shown, _ := m.data(false)
	full := prompt(m.draft, data, nil, 1, nil, "")
	for _, want := range []string{"2: second", evidenceRevision, `"start":2`, `"end":2`, "reported fact", `"d1"`} {
		if !strings.Contains(full, want) {
			t.Fatalf("missing %q: %s", want, full)
		}
	}
	for _, doc := range p.Reads {
		if doc.Path == "src/example.go" {
			t.Fatalf("seed duplicated target: %+v", doc)
		}
	}
	_, fresh, err := m.read(ctx, ReadRequest{File: "src/example.go", Mode: "lines", Start: 2, End: 2, Context: 5})
	if err != nil || fresh {
		t.Fatalf("duplicate read fresh=%v err=%v", fresh, err)
	}
	resumed := resumedPrompt(full, m.draft, m, &sessionState{ID: "session", Sent: map[string]bool{}}, shown, turnPromptState{})
	for _, want := range []string{"2: second", evidenceRevision, "reported fact", `"d1"`} {
		if !strings.Contains(resumed, want) {
			t.Fatalf("resume missing %q: %s", want, resumed)
		}
	}
}

func TestReadEvidenceInvalidSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*materials, *projectState)
	}{
		{"revision", func(m *materials, p *projectState) { p.Revision = strings.Repeat("b", 40) }},
		{"content", func(m *materials, p *projectState) {
			fleet.Object(fleet.Elements(fleet.Object(m.draft["readEvidence"])["documents"])[0])["text"] = "other"
		}},
		{"range", func(m *materials, p *projectState) {
			fleet.Object(fleet.Elements(fleet.Object(m.draft["readEvidence"])["documents"])[0])["end"] = 99
		}},
		{"denied", func(m *materials, p *projectState) {
			fleet.Object(fleet.Elements(fleet.Object(m.draft["readEvidence"])["documents"])[0])["path"] = "private/secret.txt"
		}},
		{"read failure", func(m *materials, p *projectState) { e := p.entries["src/example.go"]; e.Size++; p.entries[e.Path] = e }},
		{"outline", func(m *materials, p *projectState) {
			fleet.Object(fleet.Elements(fleet.Object(m.draft["readEvidence"])["documents"])[0])["path"] = "repo-index"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, p := evidenceFixture(t)
			tc.mutate(m, p)
			if err := m.verifyReadEvidence(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			data, _, _ := m.data(false)
			if len(m.evidence.documents) != 0 || len(m.evidence.facts) != 0 || len(m.evidence.rejected) == 0 {
				t.Fatalf("invalid evidence: %+v", m.evidence)
			}
			if !strings.Contains(fmt.Sprint(data["readEvidence"]), "reason") {
				t.Fatalf("reason missing: %v", data)
			}
		})
	}
}

func TestReadEvidenceFactRequiresEverySource(t *testing.T) {
	m, p := evidenceFixture(t)
	e := fleet.Object(m.draft["readEvidence"])
	e["documents"] = append(fleet.Elements(e["documents"]), Record{"id": "d2", "project": p.Name, "path": "src/example.go", "revision": p.Revision, "start": 1, "end": 1, "text": "wrong"})
	e["facts"] = []any{Record{"statement": "combined", "documents": []any{"d1", "d2"}}}
	if err := m.verifyReadEvidence(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.evidence.documents) != 1 || len(m.evidence.facts) != 0 || len(m.evidence.rejected) == 0 {
		t.Fatalf("partly invalid sources: %+v", m.evidence)
	}
}

func TestReadEvidenceDeliveryBoundary(t *testing.T) {
	m, p := evidenceFixture(t)
	if err := m.verifyReadEvidence(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	_, shown, bodies := m.data(false)
	m.addDeliveredBodies(nil, bodies)
	if len(bodies) != 0 {
		t.Fatalf("before delivery: %v", bodies)
	}
	m.addEvidenceShown(shown, bodies)
	if len(bodies) != 1 || !bodies["owner/app:src/example.go"] {
		t.Fatalf("after delivery: %v", bodies)
	}
	for _, tc := range []struct {
		file        string
		wantProblem bool
	}{{"src/example.go:2-2", false}, {"src/example.go:1-2", true}, {"src/example.go（全文）", true}, {"src/example.go", true}} {
		problem := m.evidenceRangeProblem(m.draft, Record{"reviewedFiles": []any{tc.file}}, shown, nil)
		if (problem != "") != tc.wantProblem {
			t.Fatalf("%s: %q", tc.file, problem)
		}
	}
	full, _, err := m.read(context.Background(), ReadRequest{File: "src/example.go", Mode: "full"})
	if err != nil || full.Reason != "" {
		t.Fatalf("full read: %+v %v", full, err)
	}
	if problem := m.evidenceRangeProblem(m.draft, Record{"reviewedFiles": []any{"src/example.go"}}, shown, nil); problem == "" {
		t.Fatal("undelivered full read accepted")
	}
	shown[documentKey(full)] = true
	if problem := m.evidenceRangeProblem(m.draft, Record{"reviewedFiles": []any{"src/example.go"}}, shown, nil); problem != "" {
		t.Fatalf("delivered full read rejected: %s", problem)
	}
	doc := m.evidence.documents[0]
	for _, tc := range []struct {
		name string
		docs []Document
		sent map[string]bool
	}{
		{"path only", nil, map[string]bool{documentKey(doc): true}},
		{"outline", []Document{{Project: doc.Project, Path: doc.Path, Request: ReadRequest{Mode: "outline"}}}, map[string]bool{}},
		{"undelivered", []Document{doc}, map[string]bool{}},
		{"outside", []Document{doc}, map[string]bool{"other range": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.evidence.documents = tc.docs
			b := map[string]bool{}
			m.addDeliveredBodies(&sessionState{ID: "session", Sent: tc.sent}, b)
			if len(b) != 0 {
				t.Fatalf("unexpected reviewed: %v", b)
			}
		})
	}
	m.evidence.documents = []Document{doc}
	bodies = map[string]bool{}
	m.state.Delivered = map[string]bool{documentKey(doc): true}
	m.addDeliveredBodies(&sessionState{Sent: map[string]bool{documentKey(doc): true}}, bodies)
	if len(bodies) != 0 {
		t.Fatalf("new conversation used old delivery: %v", bodies)
	}
}

func TestReadEvidenceRespectsReadBudget(t *testing.T) {
	m, p := evidenceFixture(t)
	p.Reads = []Document{{Project: p.Name, Path: "large.txt", Request: ReadRequest{Mode: "full"}, Text: strings.Repeat("x", maxProject-10)}}
	if err := m.verifyReadEvidence(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.evidence.documents) != 1 {
		t.Fatalf("evidence unavailable: %+v", m.evidence)
	}
	doc, _, err := m.read(context.Background(), ReadRequest{File: "src/example.go", Mode: "full", Context: 5})
	if err != nil || doc.Reason != "total" {
		t.Fatalf("project limit: %+v %v", doc, err)
	}
}

func TestReadEvidenceAbsentAndResume(t *testing.T) {
	ctx := context.Background()
	m, p := evidenceFixture(t)
	savedEvidence := m.draft["readEvidence"]
	delete(m.draft, "readEvidence")
	if err := m.bootstrap(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.seed(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range p.Reads {
		if d.Path == "src/example.go" && d.Request.Mode == "full" {
			found = true
		}
	}
	if !found {
		t.Fatal("normal seed missing")
	}
	if _, ok := func() (any, bool) { d, _, _ := m.data(false); v, ok := d["readEvidence"]; return v, ok }(); ok {
		t.Fatal("absent evidence appeared")
	}
	m, p = evidenceFixture(t)
	m.draft["readEvidence"] = savedEvidence
	if err := m.bootstrap(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.seed(ctx); err != nil {
		t.Fatal(err)
	}
	_, shown, _ := m.data(true)
	if err := m.rememberDelivered(ctx, shown); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m.draft["progress"])
	var progress map[string]any
	if err := json.Unmarshal(raw, &progress); err != nil {
		t.Fatal(err)
	}
	draft := copyRecord(m.draft)
	draft["progress"] = progress
	restored, err := m.service.newMaterials(draft, map[string]any{"repositories": []any{Record{"name": p.Name}}})
	if err != nil {
		t.Fatal(err)
	}
	restored.state.Projects[p.Name].entries = p.entries
	m = restored
	p = m.state.Projects[p.Name]
	if err := m.bootstrap(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.seed(ctx); err != nil {
		t.Fatal(err)
	}
	data, _, bodies := m.data(true)
	m.addDeliveredBodies(nil, bodies)
	if len(bodies) != 0 && bodies["owner/app:src/example.go"] {
		t.Fatal("new conversation counted before delivery")
	}
	if !strings.Contains(fmt.Sprint(data["readEvidence"]), "reported fact") {
		t.Fatalf("fact lost: %v", data)
	}
	initial := map[string]bool{}
	for _, doc := range p.Reads {
		initial[doc.Path] = true
	}
	if !initial["AGENTS.md"] || !initial["README.md"] {
		t.Fatalf("initial documents lost: %v", initial)
	}
	outside, fresh, err := m.read(ctx, ReadRequest{File: "src/example.go", Mode: "lines", Start: 1, End: 1, Context: 5})
	if err != nil || !fresh || outside.Text != "1: first" {
		t.Fatalf("outside range: %+v fresh=%v err=%v", outside, fresh, err)
	}
	_, shown, _ = m.data(false)
	if problem := m.evidenceRangeProblem(m.draft, Record{"reviewedFiles": []any{"src/example.go:1-1"}}, shown, nil); problem != "" {
		t.Fatalf("ordinary range rejected: %s", problem)
	}
	if problem := m.evidenceRangeProblem(m.draft, Record{"reviewedFiles": []any{"src/example.go:3-3"}}, shown, nil); problem == "" {
		t.Fatal("unread range counted after a different ordinary read")
	}
	p.Revision = strings.Repeat("b", 40)
	if err := m.verifyReadEvidence(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(m.evidence.documents) != 0 || len(m.evidence.facts) != 0 {
		t.Fatal("old revision reused")
	}
}
