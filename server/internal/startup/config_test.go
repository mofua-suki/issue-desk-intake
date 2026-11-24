package startup

import (
	"context"

	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 30, "hosts": map[string]any{}})
	file := filepath.Join(dir, "config.local.json")
	c := map[string]any{"workerId": "nas", "giteaUrl": "http://gitea:3000", "runtimeDir": "runtime", "tokenFile": "private/admin-token", "codex": "tools/codex", "repositories": []any{map[string]any{"name": "owner/app"}}, "intake": map[string]any{"bind": "0.0.0.0", "publicUrl": "https://desk.example"}}
	return file, dir, c
}
func writeConfig(t *testing.T, file string, value any) {
	t.Helper()
	raw, _ := json.Marshal(value)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestServerConfigResolvesPathsWithoutChangingPersistentSettings(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	declared := filepath.Join(dir, "declared.json")
	writeConfig(t, declared, map[string]any{"projectCreation": map[string]any{"owner": "owner"}})
	config, err := Load(file, dir, declared)
	if err != nil {
		t.Fatal(err)
	}
	if config.Address() != "0.0.0.0:8787" || config.RuntimeDir != filepath.Join(dir, "runtime") || config.TokenFile != filepath.Join(dir, "private", "gitea-intake-token") {
		t.Fatalf("%+v", config)
	}
	if config.Values["projectCreation"] == nil || config.PublicURL != "https://desk.example" {
		t.Fatal("declared config missing")
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "projectCreation") {
		t.Fatal("persistent settings changed")
	}
}
func TestConfigFailuresNeverEchoSecretInput(t *testing.T) {
	file, dir, value := configFixture(t)
	value["giteaUrl"] = "https://user:secret@example.test"
	writeConfig(t, file, value)
	if _, err := Load(file, dir, ""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("credential URL accepted or exposed: %v", err)
	}
	if err := os.WriteFile(file, []byte(`{"secret":"private-value",broken}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file, dir, ""); err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("JSON content echoed")
	}
}
func TestWorkerBootstrapAndOtherHostCannotStartServer(t *testing.T) {
	file, dir, value := configFixture(t)
	value["workerId"] = "other"
	writeConfig(t, file, value)
	if _, err := Load(file, dir, ""); err == nil {
		t.Fatal("other host accepted")
	}
	writeConfig(t, file, map[string]any{"workerId": "nas", "serverUrl": "https://desk.example", "serverTokenFile": "private/token"})
	if _, err := Load(file, dir, ""); err == nil {
		t.Fatal("worker bootstrap accepted")
	}
}
func TestDeclaredCredentialsAndMalformedSettingsAreRejected(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	declared := filepath.Join(dir, "declared.json")
	writeConfig(t, declared, map[string]any{"projectCreation": map[string]any{"password": "secret"}})
	if _, err := Load(file, dir, declared); err == nil {
		t.Fatal("declared credential accepted")
	}
	value["intake"].(map[string]any)["port"] = 12.5
	writeConfig(t, file, value)
	if _, err := Load(file, dir, ""); err == nil {
		t.Fatal("fractional port accepted")
	}
}

func TestMissingDedicatedCredentialDoesNotUseAdministrativeCredential(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "admin-token"), []byte("fixture-administrative-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if path, err := config.CredentialPath(); err == nil || path != "" {
		t.Fatal("missing dedicated credential fell back to administrator")
	}
}

func TestHostSSHPolicyFileDefaultsToTheDistributedFile(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Values["hostSshPolicyFile"] != filepath.Join(dir, "host-ssh.json") {
		t.Fatalf("指定が無いときは配布物の host-ssh.json を使います: %v", config.Values["hostSshPolicyFile"])
	}
	value["hostSshPolicyFile"] = "custom/host-ssh.json"
	writeConfig(t, file, value)
	config, err = Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Values["hostSshPolicyFile"] != filepath.Join(dir, "custom", "host-ssh.json") {
		t.Fatalf("指定は上書きされません: %v", config.Values["hostSshPolicyFile"])
	}
	delete(value, "hostSshPolicyFile")
	value["sshRepairPolicyFile"] = "old/ssh-repair.json"
	writeConfig(t, file, value)
	config, err = Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Values["hostSshPolicyFile"] != nil {
		t.Fatalf("旧キーの指定があるときは既定を足しません: %v", config.Values["hostSshPolicyFile"])
	}
}

func TestLoadLocalRejectsEmptyRepositories(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	if _, err := LoadLocal(file, dir, ""); err != nil {
		t.Fatalf("configured local repository rejected: %v", err)
	}
	for _, repositories := range []any{[]any{}, nil} {
		if repositories == nil {
			delete(value, "repositories")
		} else {
			value["repositories"] = repositories
		}
		writeConfig(t, file, value)
		if _, err := LoadLocal(file, dir, ""); err == nil || err.Error() != "Serverのリポジトリ一覧を設定してください" {
			t.Fatalf("repositories=%v: expected missing repository error, got %v", repositories, err)
		}
		if _, err := Load(file, dir, ""); err == nil || err.Error() != "Serverのリポジトリ一覧を設定してください" {
			t.Fatalf("repositories=%v: expected empty merged repository error, got %v", repositories, err)
		}
	}
}

func TestFleetRepositoriesStartupCandidates(t *testing.T) {
	file, _, value := configFixture(t)
	value["repositories"] = []any{}
	writeConfig(t, file, value)
	root := filepath.Join("..", "..", "..")
	c, err := Load(file, root, "")
	if err != nil {
		t.Fatal(err)
	}
	warnings := []string{}
	c.groupWarnings.emit = func(s string) { warnings = append(warnings, s) }
	shared, err := c.Shared(context.Background(), &fleet.Service{FleetPath: c.FleetPath, AssignmentsPath: c.AssignmentsPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mofua-suki/sample-expense-server", "mofua-suki/sample-expense-client"} {
		if err := c.Project(name); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, raw := range fleet.List(shared["repositories"]) {
			entry := fleet.Object(raw)
			if entry["name"] == name && fleet.Text(entry["displayName"]) != "" && fleet.Text(entry["description"]) != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing repository metadata: %s", name)
		}
	}
	groups := fleet.List(fleet.Object(shared["assignments"])["repositoryGroups"])
	found := false
	for _, raw := range groups {
		g := fleet.Strings(raw)
		if len(g) == 2 && g[0] == "mofua-suki/sample-expense-server" && g[1] == "mofua-suki/sample-expense-client" {
			found = true
		}
	}
	if !found || len(warnings) != 0 {
		t.Fatalf("groups=%v warnings=%v", groups, warnings)
	}
}

func TestFleetRepositoriesUnionAndRemoval(t *testing.T) {
	file, dir, value := configFixture(t)
	value["repositories"] = []any{map[string]any{"name": "OWNER/APP", "displayName": "local"}, map[string]any{"name": "owner/local"}, map[string]any{"name": "owner/remove"}}
	writeConfig(t, file, value)
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"pc": map[string]any{"repositories": []any{map[string]any{"name": "owner/app"}, map[string]any{"name": "owner/fleet"}, map[string]any{"name": "owner/remove"}}}}})
	c, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repositories) != 4 || c.Project("owner/local") != nil || c.Project("owner/fleet") != nil {
		t.Fatalf("candidates: %v", c.Repositories)
	}
	value["repositories"] = []any{map[string]any{"name": "OWNER/APP"}, map[string]any{"name": "owner/local"}}
	writeConfig(t, file, value)
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"pc": map[string]any{"repositories": []any{map[string]any{"name": "owner/app"}, map[string]any{"name": "owner/fleet"}}}}})
	c, err = Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repositories) != 3 || c.Project("owner/remove") == nil {
		t.Fatalf("candidates: %v", c.Repositories)
	}
}

func TestFleetRepositoriesReload(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"pc": map[string]any{"repositories": []any{map[string]any{"name": "owner/fleet"}}}}})
	c, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Config: c}
	next := fleet.Object(fleet.Clone(value))
	next["repositories"] = append(fleet.List(next["repositories"]), map[string]any{"name": "owner/new"})
	if err = a.ValidateConfig(next); err != nil {
		t.Fatal(err)
	}
	if err = a.Reload(next); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner/app", "owner/fleet", "owner/new"} {
		if err = a.Config.Project(name); err != nil {
			t.Fatal(err)
		}
	}
	altered := fleet.Object(fleet.Clone(next))
	fleet.Object(fleet.List(altered["repositories"])[0])["displayName"] = "changed"
	if a.ValidateConfig(altered) == nil || a.Reload(altered) == nil {
		t.Fatal("existing settings changed")
	}
}

func TestFleetRepositoriesReadFailure(t *testing.T) {
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	path := filepath.Join(dir, "fleet.json")
	for _, data := range []string{"", "{broken", `{"version":2,"defaults":{},"hosts":{}}`, `{"version":1,"defaults":{},"hosts":{"pc":{"repositories":[{"name":"invalid"}]}}}`} {
		if data == "" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(file, dir, ""); err == nil {
			t.Fatalf("fleet accepted: %q", data)
		}
	}
}
