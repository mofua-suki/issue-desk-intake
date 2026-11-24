package consultationai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

func groupFixture(t *testing.T, parent string, fail string) (*Service, Record, *materials) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/", 3)
		repo := strings.Join(parts[:2], "/")
		source := "# fixed repository body " + repo + "\n"
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(source), source)))
		sha := fmt.Sprintf("%x", sum)
		if fail != "" && strings.Contains(r.URL.Path, "/repos/"+fail+"/git/blobs/") {
			http.Error(w, "failed", 500)
			return w.Result(), nil
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			json.NewEncoder(w).Encode(Record{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(source))})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			json.NewEncoder(w).Encode(Record{"tree": []any{Record{"path": "README.md", "type": "blob", "mode": "100644", "sha": sha, "size": len(source)}}, "truncated": false})
		case strings.Contains(r.URL.Path, "/branches/"):
			json.NewEncoder(w).Encode(Record{"commit": Record{"id": strings.Repeat("a", 40)}})
		default:
			fmt.Fprint(w, `{"default_branch":"trunk"}`)
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{Gitea: client, Now: func() time.Time { return time.Unix(1, 0) }}}
	config := map[string]any{"repositories": []any{Record{"name": "owner/a"}, Record{"name": "owner/b"}, Record{"name": "owner/c"}}, "assignments": Record{"repositoryGroups": [][]string{{"owner/a", "owner/b"}}}}
	d := Record{"project": parent, "_ephemeral": true}
	m, err := s.newMaterials(d, config)
	if err != nil {
		t.Fatal(err)
	}
	return s, d, m
}
func groupReady() Record {
	return Record{"ready": true, "questions": []any{}, "reply": "proposal", "reviewedFiles": []any{"owner/a:README.md", "owner/b:README.md"}, "splitIssues": []any{Record{"project": "owner/a"}, Record{"project": "owner/b"}}, "crossProject": Record{"reason": "契約を整合する", "scopes": []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": "契約テスト"}}}}
}
func groupRead(t *testing.T, m *materials, parent string) {
	t.Helper()
	if err := m.bootstrap(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := m.bootstrapGroup(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, shown, _ := m.data(false)
	if err := m.rememberDelivered(context.Background(), shown); err != nil {
		t.Fatal(err)
	}
}
func TestRepositoryGroupsConsultation(t *testing.T) {
	t.Run("shipped configuration", func(t *testing.T) {
		root := filepath.Join("..", "..", "..")
		source := &fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
		_, assignments, err := source.Load()
		if err != nil {
			t.Fatal(err)
		}
		for _, parent := range []string{"mofua-suki/sample-expense-server", "mofua-suki/sample-expense-client"} {
			s, _, _ := groupFixture(t, parent, "")
			config := map[string]any{"repositories": []any{Record{"name": "mofua-suki/sample-expense-server"}, Record{"name": "mofua-suki/sample-expense-client"}}, "assignments": assignments.Output}
			m, err := s.newMaterials(Record{"project": parent, "_ephemeral": true}, config)
			if err != nil {
				t.Fatal(err)
			}
			groupRead(t, m, parent)
			if len(m.group) != 2 || len(m.state.Projects) != 2 {
				t.Fatalf("shipped group not read: %v", m.group)
			}
		}
	})
	t.Run("Turn", func(t *testing.T) {
		for _, parent := range []string{"owner/a", "owner/b"} {
			var input string
			s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
				input = request.Input
				value := reply()
				value["ready"], value["title"], value["body"], value["questions"] = true, "題名", "実装内容", []any{}
				value["reviewedFiles"] = []any{parent + ":README.md"}
				return measuredAnswer(value), nil
			})
			fixture, _, _ := groupFixture(t, parent, "")
			s.options.Gitea = fixture.options.Gitea
			original := s.options.Config
			s.options.Config = func(ctx context.Context) (map[string]any, error) {
				config, err := original(ctx)
				config["repositories"] = []any{Record{"name": "owner/a"}, Record{"name": "owner/b"}}
				fleet.Object(config["assignments"])["repositoryGroups"] = [][]string{{"owner/a", "owner/b"}}
				return config, err
			}
			d["project"] = parent
			result, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if result["ready"] != false || len(fleet.Elements(result["questions"])) == 0 {
				t.Fatalf("Turn accepted incomplete group: %v", result)
			}
			if !strings.Contains(input, `"repositoryGroup":["owner/a","owner/b"]`) || !strings.Contains(input, "fixed repository body owner/a") || !strings.Contains(input, "fixed repository body owner/b") {
				t.Fatalf("group documents not delivered: %s", input)
			}
		}
	})
	for _, parent := range []string{"owner/a", "owner/b"} {
		t.Run(parent, func(t *testing.T) {
			s, d, m := groupFixture(t, parent, "")
			if len(m.group) != 2 {
				t.Fatalf("group=%v", m.group)
			}
			groupRead(t, m, parent)
			input, _, _ := m.data(false)
			if len(fleet.Elements(input["repositoryGroup"])) != 2 || len(m.state.Projects) != 2 {
				t.Fatalf("context=%v", input)
			}
			answer := groupReady()
			answer["reviewedFiles"] = []any{parent + ":README.md"}
			if err := s.crossApproval(context.Background(), d, answer, m); err != nil {
				t.Fatal(err)
			}
			if answer["ready"] != false || len(fleet.Elements(answer["questions"])) == 0 {
				t.Fatalf("missing body accepted: %v", answer)
			}
			if _, _, err := m.read(context.Background(), ReadRequest{File: "owner/private:README.md", Mode: "full"}); err == nil {
				t.Fatal("outside allowlist read")
			}
		})
	}
	_, _, failed := groupFixture(t, "owner/a", "owner/b")
	if err := failed.bootstrap(context.Background(), "owner/a"); err != nil {
		t.Fatal(err)
	}
	if err := failed.bootstrapGroup(context.Background()); err == nil {
		t.Fatal("failed blob accepted")
	}
	if p := failed.state.Projects["owner/b"]; p != nil {
		for _, doc := range p.Reads {
			if doc.SHA256 != "" {
				t.Fatal("failed read recorded")
			}
		}
	}
	_, _, ungrouped := groupFixture(t, "owner/c", "")
	groupRead(t, ungrouped, "owner/c")
	if len(ungrouped.group) != 0 || len(ungrouped.state.Projects) != 1 {
		t.Fatal("ungrouped behavior changed")
	}
}
func TestRepositoryGroupsConsultationCoverage(t *testing.T) {
	s, d, m := groupFixture(t, "owner/a", "")
	groupRead(t, m, "owner/a")
	for _, split := range []any{[]any{}, []any{Record{"project": "owner/a"}}} {
		a := groupReady()
		a["splitIssues"] = split
		a["crossProject"] = Record{}
		if err := s.crossApproval(context.Background(), d, a, m); err != nil {
			t.Fatal(err)
		}
		if a["ready"] != false {
			t.Fatalf("incomplete plan accepted: %v", a)
		}
	}
	for _, scope := range []string{"除外なし", "除外:"} {
		a := groupReady()
		a["splitIssues"] = []any{Record{"project": "owner/a"}}
		fleet.Object(a["crossProject"])["scopes"] = []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": scope}}
		if err := s.crossApproval(context.Background(), d, a, m); err != nil {
			t.Fatal(err)
		}
		fp := fleet.Text(fleet.Object(a["crossProposal"])["fingerprint"])
		d["messages"] = []any{Record{"role": "assistant", "crossProposal": fp}, Record{"role": "user", "id": "invalid", "text": "了承します"}}
		b := groupReady()
		b["splitIssues"] = []any{Record{"project": "owner/a"}}
		fleet.Object(b["crossProject"])["scopes"] = fleet.Object(a["crossProject"])["scopes"]
		fleet.Object(b["crossProject"])["approvalMessageId"] = "invalid"
		if err := s.crossApproval(context.Background(), d, b, m); err != nil {
			t.Fatal(err)
		}
		if b["ready"] != false {
			t.Fatalf("invalid exclusion accepted: %s", scope)
		}
		d["crossApprovals"] = nil
	}
	a := groupReady()
	if err := s.crossApproval(context.Background(), d, a, m); err != nil {
		t.Fatal(err)
	}
	if a["ready"] != false || fleet.Object(a["crossProposal"])["fingerprint"] == nil {
		t.Fatalf("approval not requested: %v", a)
	}
	missingScope := groupReady()
	fleet.Object(missingScope["crossProject"])["scopes"] = []any{Record{"project": "owner/b", "scope": "契約テスト"}}
	if err := s.crossApproval(context.Background(), d, missingScope, m); err == nil {
		t.Fatal("parent scope omitted")
	}
}
func TestRepositoryGroupsConsultationApproval(t *testing.T) {
	s, d, m := groupFixture(t, "owner/a", "")
	groupRead(t, m, "owner/a")
	for _, exception := range []bool{false, true} {
		t.Run(fmt.Sprint(exception), func(t *testing.T) {
			a := groupReady()
			cross := fleet.Object(a["crossProject"])
			if exception {
				a["splitIssues"] = []any{Record{"project": "owner/a"}}
				cross["scopes"] = []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": "除外: 対応不要の条件"}}
			}
			if err := s.crossApproval(context.Background(), d, a, m); err != nil {
				t.Fatal(err)
			}
			proposal := fleet.Object(a["crossProposal"])
			fp := fleet.Text(proposal["fingerprint"])
			if fp == "" || a["ready"] != false || proposal["approved"] != false {
				t.Fatalf("approval bypassed: %v", a)
			}
			d["messages"] = []any{Record{"role": "assistant", "crossProposal": fp}, Record{"role": "user", "id": "yes", "text": "了承します"}}
			rejected := groupReady()
			rejectedCross := fleet.Object(rejected["crossProject"])
			rejectedCross["approvalMessageId"] = "yes"
			if exception {
				rejected["splitIssues"] = []any{Record{"project": "owner/a"}}
				rejectedCross["scopes"] = cross["scopes"]
			}
			fleet.Object(fleet.Elements(d["messages"])[1])["text"] = "変更してください"
			if err := s.crossApproval(context.Background(), d, rejected, m); err != nil {
				t.Fatal(err)
			}
			if rejected["ready"] != false || len(fleet.Elements(d["crossApprovals"])) != 0 {
				t.Fatal("non-consent accepted")
			}
			fleet.Object(fleet.Elements(d["messages"])[1])["text"] = "了承します"
			fleet.Object(fleet.Elements(d["messages"])[1])["groupExceptionApproval"] = Record{"fingerprint": fp, "via": "screen"}
			next := groupReady()
			nextCross := fleet.Object(next["crossProject"])
			nextCross["approvalMessageId"] = "yes"
			nextCross["scopes"] = proposal["scopes"]
			if exception {
				next["splitIssues"] = []any{Record{"project": "owner/a"}}
			}
			if err := s.crossApproval(context.Background(), d, next, m); err != nil {
				t.Fatal(err)
			}
			if next["ready"] != true || fleet.Object(next["crossProposal"])["approved"] != true {
				t.Fatalf("approved plan held or display not updated: %v", next)
			}
			if exception {
				changedCondition := groupReady()
				changedCondition["splitIssues"] = []any{Record{"project": "owner/a"}}
				changedCross := fleet.Object(changedCondition["crossProject"])
				changedCross["scopes"] = []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": "除外: 別の条件"}}
				changedCross["approvalMessageId"] = "yes"
				if err := s.crossApproval(context.Background(), d, changedCondition, m); err != nil {
					t.Fatal(err)
				}
				if changedCondition["ready"] != false {
					t.Fatal("changed exception condition reused consent")
				}
			}
			changed := groupReady()
			changedCross := fleet.Object(changed["crossProject"])
			changedCross["reason"] = "別の理由"
			changedCross["approvalMessageId"] = "yes"
			if exception {
				changed["splitIssues"] = []any{Record{"project": "owner/a"}}
				changedCross["scopes"] = cross["scopes"]
			}
			if err := s.crossApproval(context.Background(), d, changed, m); err != nil {
				t.Fatal(err)
			}
			if changed["ready"] != false {
				t.Fatal("changed reason reused approval")
			}
			reverted := groupReady()
			if exception {
				reverted["splitIssues"] = []any{Record{"project": "owner/a"}}
				fleet.Object(reverted["crossProject"])["scopes"] = cross["scopes"]
			}
			if err := s.crossApproval(context.Background(), d, reverted, m); err != nil {
				t.Fatal(err)
			}
			if reverted["ready"] != false {
				t.Fatal("reverted proposal reused stale approval")
			}
			d["crossApprovals"] = nil
		})
	}
	a := groupReady()
	a["splitIssues"] = append(fleet.Elements(a["splitIssues"]), Record{"project": "owner/c"})
	cross := fleet.Object(a["crossProject"])
	cross["scopes"] = append(fleet.Elements(cross["scopes"]), Record{"project": "owner/c", "scope": "連携変更"})
	if err := s.crossApproval(context.Background(), d, a, m); err != nil {
		t.Fatal(err)
	}
	if a["ready"] != false || fleet.Object(a["crossProposal"])["fingerprint"] == nil {
		t.Fatal("group-external target bypassed approval")
	}
	fingerprint := fleet.Text(fleet.Object(a["crossProposal"])["fingerprint"])
	d["messages"] = []any{Record{"role": "assistant", "crossProposal": fingerprint}, Record{"role": "user", "id": "outside", "text": "了承します", "groupExceptionApproval": Record{"fingerprint": fingerprint, "via": "screen"}}}
	mixed := groupReady()
	mixed["splitIssues"] = a["splitIssues"]
	mixedCross := fleet.Object(mixed["crossProject"])
	mixedCross["scopes"] = fleet.Object(a["crossProject"])["scopes"]
	mixedCross["approvalMessageId"] = "outside"
	if err := s.crossApproval(context.Background(), d, mixed, m); err != nil {
		t.Fatal(err)
	}
	if mixed["ready"] != true {
		t.Fatalf("approved mixed proposal held: %v", mixed)
	}
	changed := groupReady()
	changed["splitIssues"] = a["splitIssues"]
	fleet.Object(changed["crossProject"])["scopes"] = []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": "契約テスト"}, Record{"project": "owner/c", "scope": "別の変更"}}
	if err := s.crossApproval(context.Background(), d, changed, m); err != nil {
		t.Fatal(err)
	}
	if changed["ready"] != false {
		t.Fatal("changed external scope reused consent")
	}
}
