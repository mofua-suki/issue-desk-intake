package consultationai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/gitea"
)

func TestOmittedFileRemainsAccessibleFromFixedRevision(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	ctx := context.Background()
	config, _ := s.options.Config(ctx)
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	target := "src/package-0699/long-file-name.go"
	tree := make([]any, 0, 700)
	blobs := map[string]string{}
	var targetSHA string
	for i := 0; i < 700; i++ {
		name := fmt.Sprintf("src/package-%04d/long-file-name.go", i)
		body := "package fixture\nneedle " + name
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(body), body)))
		sha := fmt.Sprintf("%x", sum)
		blobs[sha] = body
		tree = append(tree, map[string]any{"path": name, "type": "blob", "mode": "100644", "sha": sha, "size": len(body)})
		if name == target {
			targetSHA = sha
		}
	}
	blobReads := map[string]int{}
	var blobReadsMu sync.Mutex
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/owner/app"):
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		case strings.Contains(r.URL.Path, "/branches/"):
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"id": revision}})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			if !strings.HasSuffix(r.URL.Path, "/"+revision) {
				t.Errorf("tree revision: %s", r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{"tree": tree, "truncated": false})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := blobs[sha]
			if !ok {
				http.Error(w, "missing", 404)
				break
			}
			blobReadsMu.Lock()
			blobReads[sha]++
			blobReadsMu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(body))})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "missing", 404)
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.options.Gitea = client
	p, err := m.project(ctx, "owner/app")
	if err != nil {
		t.Fatal(err)
	}
	data, _, bodies := m.data(false)
	files := fleet.Object(fleet.Elements(data["projects"])[0])["availableFiles"].([]string)
	for _, name := range files {
		if name == target {
			t.Fatal("対象が省略されていない")
		}
	}
	if bodies["owner/app:"+target] {
		t.Fatal("未読のパスが既読扱い")
	}
	read := func(req ReadRequest) Document {
		t.Helper()
		doc, _, err := m.read(ctx, req)
		if err != nil || doc.Reason != "" {
			t.Fatalf("read %+v: %+v %v", req, doc, err)
		}
		return doc
	}
	index := read(ReadRequest{File: "repo-index", Mode: "lines", Start: 700, End: 700})
	if !strings.Contains(index.Text, target) {
		t.Fatalf("index: %+v", index)
	}
	doc := read(ReadRequest{File: target, Mode: "full"})
	if !strings.Contains(doc.Text, "needle "+target) || doc.Version != p.Revision || blobReads[targetSHA] != 1 {
		t.Fatalf("direct: %+v blob reads=%d", doc, blobReads[targetSHA])
	}
	// Reload materials and reuse the verified blob from the fixed tree.
	m, err = s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	search := read(ReadRequest{File: "repo-search", Mode: "keyword", Keyword: target})
	if !strings.Contains(search.Text, target+":2:") || search.Version != revision || blobReads[targetSHA] != 1 {
		t.Fatalf("search: %+v blob reads=%d", search, blobReads[targetSHA])
	}
	reads := 0
	for _, count := range blobReads {
		reads += count
	}
	second := read(ReadRequest{File: "repo-search", Mode: "keyword", Keyword: "package fixture"})
	after := 0
	for _, count := range blobReads {
		after += count
	}
	if second.Version != revision || !strings.Contains(second.Text, "src/package-0000/long-file-name.go:1: package fixture") || reads != 700 || after != reads {
		t.Fatalf("second search: version=%s reads=%d after=%d", second.Version, reads, after)
	}
}

func TestReadSuffixCandidatesAndRestore(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	ctx := context.Background()
	config, _ := s.options.Config(ctx)
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.project(ctx, "owner/app")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture supplies bodies only when an exact path is read.
	p.entries = map[string]treeEntry{}
	contents := map[string]string{}
	for _, name := range []string{"server/internal/startup/business.go", "server/internal/startup/screen.go", "server/internal/startup/notbusiness.go", "private/startup/hidden.go", "server/private/startup/business.go"} {
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(name), name)))
		sha := fmt.Sprintf("%x", sum)
		p.entries[name] = treeEntry{Path: name, SHA: sha, Size: len(name)}
		contents[sha] = name
	}
	fixture, _ := blobFixture(t, func(r *http.Request) (string, string) {
		sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		return "base64", base64.StdEncoding.EncodeToString([]byte(contents[sha]))
	})
	s.options.Gitea = fixture.options.Gitea
	read := func(file string) Document {
		t.Helper()
		doc, _, err := m.read(ctx, ReadRequest{File: file, Mode: "full", Context: 5})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	one := read("startup/business.go")
	if one.Reason != "missing" || !reflect.DeepEqual(one.Candidates, []string{"server/internal/startup/business.go"}) || one.Text != "" {
		t.Fatalf("single: %+v", one)
	}
	exact := read(one.Candidates[0])
	if exact.Reason != "" || exact.Text != one.Candidates[0] || len(exact.Candidates) != 0 {
		t.Fatalf("reread: %+v", exact)
	}
	p.entries["other/startup/business.go"] = treeEntry{Path: "other/startup/business.go"}
	many := read("owner/app:startup/business.go")
	if !reflect.DeepEqual(many.Candidates, []string{"other/startup/business.go", "server/internal/startup/business.go"}) || many.Text != "" {
		t.Fatalf("multiple: %+v", many)
	}
	for _, file := range []string{"startup/absent.go", "business.goX", "business.go"} {
		doc := read(file)
		if file == "business.go" { // The filename is itself a complete path component.
			if len(doc.Candidates) != 2 {
				t.Fatalf("component: %+v", doc)
			}
		} else if doc.Reason != "missing" || len(doc.Candidates) != 0 {
			t.Fatalf("missing: %+v", doc)
		}
	}
	if doc := read("private/startup/business.go"); doc.Reason != "denied" || len(doc.Candidates) != 0 {
		t.Fatalf("denied request: %+v", doc)
	}
	if doc := read("startup/hidden.go"); doc.Reason != "missing" || len(doc.Candidates) != 0 {
		t.Fatalf("denied candidate: %+v", doc)
	}
	// Exercise the production JSON persistence and bootstrap path for refused reads.
	saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	m, err = s.newMaterials(saved, config)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the fixed tree without making another network call in this fixture.
	m.state.Projects["owner/app"].entries = p.entries
	if err := m.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	again := read("owner/app:startup/business.go")
	if !reflect.DeepEqual(again.Candidates, many.Candidates) || !strings.Contains(string(marshal(again)), "server/internal/startup/business.go") {
		t.Fatalf("restored: %+v", again)
	}
	data, _, _ := m.data(true)
	if !strings.Contains(string(marshal(data)), `"candidates":["other/startup/business.go","server/internal/startup/business.go"]`) {
		t.Fatalf("候補がAIへの再提示に含まれない: %s", marshal(data))
	}
}

func TestReadManyCandidatesPersistsOmittedCount(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
	ctx := context.Background()
	config, _ := s.options.Config(ctx)
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.project(ctx, "owner/app")
	if err != nil {
		t.Fatal(err)
	}
	p.entries = map[string]treeEntry{}
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("dir%02d/main.go", i)
		p.entries[name] = treeEntry{Path: name}
	}
	p.entries["private/main.go"] = treeEntry{Path: "private/main.go"}
	doc, _, err := m.read(ctx, ReadRequest{File: "main.go", Mode: "full", Context: 5})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Reason != "missing" || len(doc.Candidates) != 20 || doc.Candidates[0] != "dir00/main.go" || doc.Candidates[19] != "dir19/main.go" || doc.CandidatesOmitted != 5 {
		t.Fatalf("unexpected candidates: %+v", doc)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	m, err = s.newMaterials(saved, config)
	if err != nil {
		t.Fatal(err)
	}
	m.state.Projects["owner/app"].entries = p.entries
	if err := m.bootstrap(ctx, "owner/app"); err != nil {
		t.Fatal(err)
	}
	data, _, _ := m.data(true)
	if !strings.Contains(string(marshal(data)), `"candidatesOmitted":5`) || strings.Contains(string(marshal(data)), "dir20/main.go") {
		t.Fatalf("unexpected prompt data: %s", marshal(data))
	}
	// An older saved read can contain all candidates; loading it must also bound the prompt.
	old := &m.state.Projects["owner/app"].Reads[0]
	for i := 20; i < 25; i++ {
		old.Candidates = append(old.Candidates, fmt.Sprintf("dir%02d/main.go", i))
	}
	old.CandidatesOmitted = 0
	if err := m.persist(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	m, err = s.newMaterials(saved, config)
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ = m.data(true)
	if !strings.Contains(string(marshal(data)), `"candidatesOmitted":5`) || strings.Contains(string(marshal(data)), "dir20/main.go") {
		t.Fatalf("old saved read: %s", marshal(data))
	}
}

func TestReadLimitRefusalDoesNotDeliverBody(t *testing.T) {
	for _, reason := range []string{"file", "total"} {
		t.Run(reason, func(t *testing.T) {
			const rejected = "REFUSED-CONTENT"
			body := "first line\n" + rejected + strings.Repeat("x", maxFile)
			prior := []Document{}
			if reason == "total" {
				body = body[:1000]
				for i := 0; i < 4; i++ {
					name := fmt.Sprintf("accepted-%d.txt", i)
					prior = append(prior, Document{Path: name, Request: ReadRequest{File: name, Mode: "full"}, Text: strings.Repeat("a", maxProject/4-100)})
				}
			}
			fixture, _ := blobFixture(t, func(*http.Request) (string, string) {
				return "base64", encoded(body)
			})
			project, file := "owner/app", "article.md"
			p := &projectState{Name: project, Branch: "trunk", Revision: strings.Repeat("a", 40), Reads: prior, entries: map[string]treeEntry{file: {Path: file, Size: len(body), SHA: fixtureSHA(body)}}}
			m := &materials{service: fixture, draft: Record{"project": project, "_ephemeral": true}, allowed: map[string]bool{project: true}, state: contextState{Projects: map[string]*projectState{project: p}}}

			read := func(end int) Document {
				t.Helper()
				d, _, err := m.read(t.Context(), ReadRequest{File: file, Mode: "lines", Start: 1, End: end})
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			doc := read(2)
			if doc.Reason != reason || doc.Size <= 0 || doc.Text != "" {
				t.Fatalf("read reason=%s size=%d text length=%d", doc.Reason, doc.Size, len(doc.Text))
			}
			if short := read(1); short.Reason != "" || short.Text != "1: first line" {
				t.Fatalf("small read: %+v", short)
			}
			for _, compact := range []bool{false, true} {
				data, delivered, _ := m.data(compact)
				json := string(marshal(data))
				resume := resumedPrompt("", m.draft, m, &sessionState{ID: "session"}, delivered, turnPromptState{})
				for _, input := range []string{json, resume} {
					if strings.Contains(input, rejected) || !strings.Contains(input, "1: first line") || !strings.Contains(input, `"reason":"`+reason+`"`) {
						t.Fatalf("input compact=%t has rejected text=%t; reason=%s", compact, strings.Contains(input, rejected), reason)
					}
				}
			}
		})
	}
}
