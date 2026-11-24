package consultationai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

func TestCarriedBootstrapOnceAndRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "restore retry"}[fail], func(t *testing.T) {
			var inputs []string
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				inputs = append(inputs, r.Input)
				return measuredAnswer(reply()), nil
			})
			cfg, _ := s.options.Config(t.Context())
			m, err := s.newMaterials(d, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.bootstrap(t.Context(), "owner/app"); err != nil {
				t.Fatal(err)
			}
			if _, _, err = m.read(t.Context(), ReadRequest{File: "open-requests", Mode: "full", Context: 5}); err != nil {
				t.Fatal(err)
			}
			if err = m.persist(t.Context()); err != nil {
				t.Fatal(err)
			}
			var carried map[string]any
			if err = json.Unmarshal(marshal(d["progress"]), &carried); err != nil {
				t.Fatal(err)
			}
			d["carriedContext"], d["progress"] = carried, nil
			calls := 0
			s.options.Reference = func(_ context.Context, _ Record, name string) (Reference, error) {
				calls++
				if fail && calls == 1 {
					return Reference{}, errors.New("restore failure")
				}
				return Reference{Version: "latest", Text: "最新の候補本文"}, nil
			}
			if fail {
				if _, err = s.Turn(t.Context(), d); err == nil {
					t.Fatal("expected restoration failure")
				}
				calls = 0
				fail = false
			}
			if _, err = s.Turn(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(inputs) != 1 || !strings.Contains(inputs[0], "最新の候補本文") {
				t.Fatalf("reference calls=%d inputs=%v", calls, inputs)
			}
		})
	}
}

func TestCanonicalReadsAndEvidence(t *testing.T) {
	m, p := evidenceFixture(t)
	contents := map[string]string{fixtureSHA("first\nsecond\nthird\n"): "first\nsecond\nthird\n", fixtureSHA("intro\n"): "intro\n"}
	service, calls := blobFixture(t, func(r *http.Request) (string, string) {
		return "base64", encoded(contents[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	m.service = service
	ctx := t.Context()
	if err := m.verifyReadEvidence(ctx, p); err != nil {
		t.Fatal(err)
	}
	requests := []any{Record{"file": "README.md", "mode": "full", "context": 5}, Record{"file": "owner/app:README.md", "mode": "full", "context": 0}}
	fresh, err := m.fresh(requests)
	if err != nil || len(fresh) != 1 {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	r, _ := request(fresh[0])
	first, added, err := m.read(ctx, r)
	if err != nil || !added {
		t.Fatalf("first=%+v added=%v err=%v", first, added, err)
	}
	used := projectBytes(p.Reads)
	alias, added, err := m.read(ctx, ReadRequest{File: "owner/app:README.md", Mode: "full", Context: 0})
	if err != nil || added || alias.Text != first.Text || projectBytes(p.Reads) != used || calls.Load() != 2 {
		t.Fatalf("alias=%+v added=%v calls=%d err=%v", alias, added, calls.Load(), err)
	}
	next, err := m.fresh(requests)
	if err != nil || len(next) != 0 || len(p.Reads) != 1 {
		t.Fatalf("next=%v reads=%+v err=%v", next, p.Reads, err)
	}
	keywords := []any{
		Record{"file": "README.md", "mode": "keyword", "keyword": "intro", "context": 0},
		Record{"file": "README.md", "mode": "keyword", "keyword": "intro", "context": 5},
	}
	if next, err := m.fresh(keywords); err != nil || len(next) != 2 {
		t.Fatalf("different search windows were combined: %v, %v", next, err)
	}

	variants := []any{
		Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1, "context": 0},
		Record{"file": "owner/app:README.md", "mode": "lines", "start": 1, "end": 1, "context": 5},
		Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1, "keyword": "ignored"},
		Record{"file": "README.md", "mode": "outline", "context": 5},
		Record{"file": "README.md", "mode": "outline", "start": 1, "context": 0, "keyword": "ignored"},
		Record{"file": "README.md", "mode": "keyword", "keyword": "intro"},
		Record{"file": "src/example.go", "mode": "lines", "start": 2, "end": 2},
		Record{"file": "missing.go", "mode": "full"},
	}
	fresh, err = m.fresh(variants)
	if err != nil || len(fresh) != 4 {
		t.Fatalf("variants=%v err=%v", fresh, err)
	}
	for _, v := range fresh {
		r, err := request(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = m.read(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected refetches: %d", calls.Load())
	}
	if len(p.Reads) != 5 {
		t.Fatalf("reads=%+v", p.Reads)
	}
	retry, err := m.fresh(variants)
	if err != nil || len(retry) != 1 {
		t.Fatalf("refused retry=%v err=%v", retry, err)
	}
	missing, err := request(retry[0])
	if err != nil || missing.File != "missing.go" {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	_, shown, _ := m.data(false)
	for _, doc := range p.Reads {
		if doc.Reason != "" && shown[documentKey(doc)] {
			t.Fatalf("refusal delivered: %+v", doc)
		}
	}
}

func TestEstimateScopeResumeAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "fallback"}[fallback], func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			calls := 0
			s, _ := testServiceSource(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if calls == 1 {
					if r.Resume || r.OnSession == nil {
						t.Fatal("initial session missing")
					}
					if err := r.OnSession("11111111-1111-4111-8111-111111111112"); err != nil {
						t.Fatal(err)
					}
					return measuredAnswer(Record{"readFiles": []any{Record{"file": "README.md", "mode": "lines", "start": 2, "end": 2}}}), nil
				}
				if calls == 2 {
					if !r.Resume {
						t.Fatal("expected resume")
					}
					_, raw, ok := strings.Cut(r.Input, "\n")
					var payload Record
					if !ok || json.Unmarshal([]byte(raw), &payload) != nil {
						t.Fatal(r.Input)
					}
					docs := fleet.Elements(payload["documents"])
					if fleet.Number(payload["remainingReadRounds"]) != 2 || len(docs) != 1 || !strings.Contains(string(marshal(docs)), "second") || strings.Contains(r.Input, "first") || payload["issue"] != nil || payload["context"] != nil {
						t.Fatalf("resume payload=%v", payload)
					}
					if fallback {
						answer := measuredAnswer(nil)
						answer.Diagnostic = ai.Diagnostic{ID: "failed", Kind: "execution"}
						return answer, &ai.Failure{Kind: "execution"}
					}
				}
				if calls == 3 {
					if r.Resume || !strings.Contains(r.Input, "issue fixture") || !strings.Contains(r.Input, "first") || !strings.Contains(r.Input, "second") {
						t.Fatalf("fallback input=%s", r.Input)
					}
				}
				return measuredAnswer(Record{"readFiles": []any{}, "reviewedFiles": []any{"README.md"}, "changeScope": Record{"known": true, "paths": []any{"README.md"}}}), nil
			}, "first\nsecond\nthird\n")
			result, err := s.EstimateScope(t.Context(), "owner/app", map[string]any{"body": "issue fixture"})
			want := 2
			if fallback {
				want = 3
			}
			if err != nil || calls != want || result == nil {
				t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
			}
		})
	}
}

func TestCheckpointKeepsReadMetadataWithoutDocumentBody(t *testing.T) {
	request := ReadRequest{File: "docs/spec.md", Mode: "full", Context: 5}
	read := Document{
		Request: request, Project: "owner/app", Path: request.File,
		Text: strings.Repeat("document body\n", 9000), SHA256: "stored digest",
		Candidates: []string{"docs/example.md"},
	}
	project := &projectState{
		Name: "owner/app", Branch: "trunk", Revision: strings.Repeat("a", 40),
		Reads: []Document{read}, entries: map[string]treeEntry{"docs/spec.md": {Type: "blob"}},
	}
	draft := Record{"_ephemeral": true, "project": "owner/app"}
	m := &materials{
		service: &Service{}, draft: draft,
		state: contextState{
			Projects: map[string]*projectState{"owner/app": project},
			Pending:  []ReadRequest{request}, Delivered: map[string]bool{"delivered": true},
			Rounds: 3,
		},
	}
	if err := m.persist(t.Context()); err != nil {
		t.Fatal(err)
	}
	if project.Reads[0].Text != read.Text || project.entries[request.File].Type != "blob" {
		t.Fatal("saving the checkpoint changed the active read")
	}
	project.Reads[0].Candidates[0] = "updated.md"
	m.state.Pending[0].File = "next.md"
	delete(m.state.Delivered, "delivered")

	saved := draft["progress"].(contextState)
	savedRead := saved.Projects["owner/app"].Reads[0]
	if savedRead.Text != "" || savedRead.SHA256 != read.SHA256 || savedRead.Request != request ||
		len(savedRead.Candidates) != 1 || savedRead.Candidates[0] != "docs/example.md" ||
		saved.Pending[0] != request || !saved.Delivered["delivered"] || saved.Rounds != 3 {
		t.Fatalf("checkpoint lost read metadata: %+v", savedRead.Request)
	}
}
