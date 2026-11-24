package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func assignmentsBody(t *testing.T) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":2,"max":1}}},"other":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{"owner/repo":{"deployHost":"nas","canaryHost":"other"}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAssignmentsPreserveRankCapacityAndRejectAmbiguity(t *testing.T) {
	raw := assignmentsBody(t)
	a, err := NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Implementers["owner/repo"], ",") != "other,nas" || a.RepoLimit("nas", "OWNER/REPO") != 1 || a.DeployHosts["owner/repo"] != "nas" {
		t.Fatalf("assignment normalization lost boundaries: %+v", a)
	}
	Object(Object(raw["hosts"])["other"])["repositories"] = map[string]any{"owner/repo": map[string]any{"rank": float64(2)}}
	if _, err = NormalizeAssignments(raw); err == nil {
		t.Fatal("duplicate repository rank accepted")
	}
}

func TestFleetRejectsSecretsAndUntrustedReleasePolicy(t *testing.T) {
	for _, config := range []map[string]any{
		{"releasePolicy": map[string]any{"permissionFiles": []any{}}},
		{"tokenFile": "/private/token", "implementation": map[string]any{"password": "hidden"}},
		{"giteaUrl": "https://user:secret@example.test"},
		{"repositories": []any{map[string]any{"name": "owner/repo", "note": "idk_" + strings.Repeat("a", 43)}}},
	} {
		if checkConfig(config) == nil {
			t.Fatal("unsafe fleet accepted")
		}
	}
	if err := checkConfig(map[string]any{"tokenFile": "/private/token", "workerTokenFile": "/private/worker-token"}); err != nil {
		t.Fatal("credential file locations rejected")
	}
}

func TestAgentPolicyAndSharedModelsRemainAuthoritative(t *testing.T) {
	config := map[string]any{"review": map[string]any{"provider": "codex"}, "model": "old", "priority": map[string]any{"provider": "codex"}}
	models := map[string]any{"model": "configured", "review": map[string]any{"claudeModel": "review-model"}}
	if err := applyModels(config, models); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "configured" || Object(config["review"])["provider"] != "opposite" || Object(config["priority"])["provider"] != "consultation" {
		t.Fatal("agent policy was overridden")
	}
	if config["claudeModel"] != nil {
		t.Fatal("unset shared model retained an old local value")
	}
}

func TestSharedModelShapesAndLimitBoundaries(t *testing.T) {
	for _, models := range []map[string]any{{"model": true}, {"intake": map[string]any{"tiers": map[string]any{"light": map[string]any{"provider": "claude"}}}}, {"reasoningEffort": "infinite"}, {"antigravity": map[string]any{"model": "gpt-6"}}} {
		raw := assignmentsBody(t)
		raw["models"] = models
		if _, e := NormalizeAssignments(raw); e == nil {
			t.Fatal(models)
		}
	}
	raw := assignmentsBody(t)
	raw["implementationUsageLimit"] = float64(9007199254740991)
	if _, e := NormalizeAssignments(raw); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"implementationRunLimit", "reviewRoundLimit", "sizeFixRounds"} {
		raw = assignmentsBody(t)
		raw[key] = float64(0)
		if _, e := NormalizeAssignments(raw); e == nil {
			t.Fatal(key)
		}
	}
}
func TestScopeExemptionsAllowExactFilesOnly(t *testing.T) {
	for _, path := range []string{"*", "src/", "../a.go", ".git/config", "NUL.txt"} {
		raw := assignmentsBody(t)
		raw["dispatch"] = map[string]any{"scopeExemptPaths": []any{path}}
		if _, e := NormalizeAssignments(raw); e == nil {
			t.Fatal(path)
		}
	}
	raw := assignmentsBody(t)
	raw["dispatch"] = map[string]any{"scopeExemptPaths": []any{"./a.go", "a.go"}, "scopeExemptRepositories": map[string]any{"OWNER/REPO": []any{"b.go"}}}
	a, e := NormalizeAssignments(raw)
	if e != nil || strings.Join(a.Exempt("owner/repo"), ",") != "a.go,b.go" {
		t.Fatal(a, e)
	}
}

func TestMonitorAndCanaryConfigurationLoad(t *testing.T) {
	load := func(t *testing.T, raw map[string]any) (*Assignments, error) {
		t.Helper()
		dir := t.TempDir()
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "assignments.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "fleet.json"), []byte(`{"version":1,"hosts":{},"defaults":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, a, err := (&Service{FleetPath: filepath.Join(dir, "fleet.json"), AssignmentsPath: filepath.Join(dir, "assignments.json")}).Load()
		return a, err
	}
	base, err := load(t, assignmentsBody(t))
	if err != nil || base.MonitorHost != "" || len(base.ReleaseCanaryHosts) != 0 {
		t.Fatalf("unset: %+v %v", base, err)
	}
	raw := assignmentsBody(t)
	raw["monitorHost"] = "nas"
	raw["releaseCanaryHosts"] = []any{"other", "nas"}
	configured, err := load(t, raw)
	if err != nil || configured.MonitorHost != "nas" || !reflect.DeepEqual(configured.ReleaseCanaryHosts, []string{"other", "nas"}) {
		t.Fatalf("configured: %+v %v", configured, err)
	}
	if !reflect.DeepEqual(base.Output, configured.Output) {
		t.Fatal("new settings changed distributed Output")
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"monitorHost", nil}, {"monitorHost", 1}, {"monitorHost", "bad name"}, {"monitorHost", "missing"},
		{"releaseCanaryHosts", nil}, {"releaseCanaryHosts", "nas"}, {"releaseCanaryHosts", []any{}},
		{"releaseCanaryHosts", []any{1}}, {"releaseCanaryHosts", []any{"bad name"}},
		{"releaseCanaryHosts", []any{"missing"}}, {"releaseCanaryHosts", []any{"nas", "nas"}},
	} {
		t.Run(fmt.Sprintf("%s_%v", tc.key, tc.value), func(t *testing.T) {
			raw := assignmentsBody(t)
			raw[tc.key] = tc.value
			if _, err := load(t, raw); err == nil {
				t.Fatalf("accepted %s=%v", tc.key, tc.value)
			}
		})
	}
}

func TestSharedModelDisableLightTier(t *testing.T) {
	for _, value := range []any{true, false} {
		config := map[string]any{"implementation": map[string]any{"disableLightTier": !value.(bool)}}
		if err := applyModels(config, map[string]any{"implementation": map[string]any{"disableLightTier": value}}); err != nil {
			t.Fatal(err)
		}
		if Object(config["implementation"])["disableLightTier"] != value {
			t.Fatal(config)
		}
		if err := applyModels(config, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if _, ok := Object(config["implementation"])["disableLightTier"]; ok {
			t.Fatal(config)
		}
	}
	for _, value := range []any{"true", 1, nil} {
		if _, err := normalizeModels(map[string]any{"implementation": map[string]any{"disableLightTier": value}}); err == nil {
			t.Fatal(value)
		}
	}
}

func TestFleetEnvironmentValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   any
		valid bool
	}{
		{"legacy", nil, true},
		{"unknown", map[string]any{"platform": nil, "gcc": nil, "cgo": nil, "race": nil, "docker": nil, "realDevice": nil, "liveMeasurement": nil, "network": nil}, true},
		{"known", map[string]any{"platform": "linux", "gcc": true, "network": "allowlist"}, true},
		{"boolean type", map[string]any{"gcc": "true"}, false},
		{"windows", map[string]any{"platform": "windows"}, false},
		{"darwin", map[string]any{"platform": "darwin"}, true},
		{"platform type", map[string]any{"platform": []any{}}, false},
		{"network value", map[string]any{"network": "all"}, false},
		{"unknown field", map[string]any{"other": nil}, false},
		{"object type", "linux", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{}
			if tc.env != nil {
				config["environment"] = tc.env
			}
			err := checkConfig(config)
			if tc.name == "windows" && (err == nil || !strings.Contains(err.Error(), "platform")) {
				t.Fatalf("Linuxコンテナの理由がない: %v", err)
			}
			got := err == nil
			if got != tc.valid {
				t.Fatalf("valid = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestFleetRegisteredPlatforms(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	document, _, err := (&Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Hosts) != 8 {
		t.Fatalf("hosts = %d, want 8", len(document.Hosts))
	}
	for name, config := range document.Hosts {
		want := "linux"
		// Macネイティブのテスト専用ID（docs/mac-roles.md）。
		if name == "macbook-pro-test" || name == "macbook-air-test" {
			want = "darwin"
		}
		if got := Object(config["environment"])["platform"]; got != want {
			t.Errorf("%s platform = %v, want %s", name, got, want)
		}
	}
}

func TestAssignmentsRejectTestPlacement(t *testing.T) {
	raw := assignmentsBody(t)
	if _, err := NormalizeAssignments(raw); err != nil {
		t.Fatalf("項目なし: %v", err)
	}
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"empty", []any{}}, {"array", []any{map[string]any{"os": "darwin"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := assignmentsBody(t)
			raw["testPlacement"] = tc.value
			_, err := NormalizeAssignments(raw)
			if err == nil || !strings.Contains(err.Error(), "現行WorkerはtestPlacementを実装していません") {
				t.Fatalf("拒否理由: %v", err)
			}
		})
	}
}

func loadFixture(t *testing.T) (*Service, string, string) {
	t.Helper()
	d := t.TempDir()
	f := filepath.Join(d, "fleet.json")
	a := filepath.Join(d, "assignments.json")
	fj := `{"version":1,"defaults":{"repositories":[{"name":"owner/repo"}]},"hosts":{"nas":{"model":"one"}}}`
	aj := `{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{"owner/repo":{"deployHost":"nas"}}}`
	for p, s := range map[string]string{f: fj, a: aj} {
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{FleetPath: f, AssignmentsPath: a}, fj, aj
}
func replaceLoadFile(t *testing.T, p, s string, tm time.Time) {
	t.Helper()
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(p, tm, tm); e != nil {
		t.Fatal(e)
	}
}
func TestLoadReusesParsedFiles(t *testing.T) {
	s, orig, _ := loadFixture(t)
	first, _, e := s.Load()
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(s.FleetPath)
	if e != nil {
		t.Fatal(e)
	}
	replaceLoadFile(t, s.FleetPath, strings.Replace(orig, "one", "two", 1), info.ModTime())
	second, _, e := s.Load()
	if e != nil || !reflect.DeepEqual(first, second) || second.Hosts["nas"]["model"] != "one" {
		t.Fatalf("%v %v", second, e)
	}
}
func TestLoadReflectsFileChanges(t *testing.T) {
	s, orig, assign := loadFixture(t)
	if _, _, e := s.Load(); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(s.FleetPath)
	if e != nil {
		t.Fatal(e)
	}
	changed := strings.Replace(orig, "one", "two", 1)
	// Equal modification time and size deliberately retain the cached value.
	replaceLoadFile(t, s.FleetPath, changed, info.ModTime())
	doc, _, e := s.Load()
	if e != nil || doc.Hosts["nas"]["model"] != "one" {
		t.Fatalf("%v %v", doc, e)
	}
	replaceLoadFile(t, s.FleetPath, changed, info.ModTime().Add(time.Second))
	doc, _, e = s.Load()
	if e != nil || doc.Hosts["nas"]["model"] != "two" {
		t.Fatalf("%v %v", doc, e)
	}
	ai, e := os.Stat(s.AssignmentsPath)
	if e != nil {
		t.Fatal(e)
	}
	replaceLoadFile(t, s.AssignmentsPath, strings.Replace(assign, `"pollSeconds":300`, `"pollSeconds":60`, 1), ai.ModTime())
	_, a, e := s.Load()
	if e != nil || a.Output["pollSeconds"] != float64(60) {
		t.Fatalf("%v %v", a, e)
	}
}
func TestLoadResultIsolated(t *testing.T) {
	s, _, _ := loadFixture(t)
	d, a, e := s.Load()
	if e != nil {
		t.Fatal(e)
	}
	Object(List(d.Defaults["repositories"])[0])["name"] = "changed"
	d.Hosts["nas"]["model"] = "changed"
	a.Hosts["nas"].Repositories["owner/repo"] = RepositoryRank{}
	a.Implementers["owner/repo"][0] = "changed"
	a.Output["pollSeconds"] = 30
	d, a, e = s.Load()
	if e != nil || Object(List(d.Defaults["repositories"])[0])["name"] != "owner/repo" || d.Hosts["nas"]["model"] != "one" || a.Hosts["nas"].Repositories["owner/repo"].Rank != 1 || a.Implementers["owner/repo"][0] != "nas" || a.Output["pollSeconds"] != float64(300) {
		t.Fatalf("%v %v %v", d, a, e)
	}
}
func TestLoadDoesNotCacheFailure(t *testing.T) {
	s, _, assign := loadFixture(t)
	info, e := os.Stat(s.AssignmentsPath)
	if e != nil {
		t.Fatal(e)
	}
	replaceLoadFile(t, s.AssignmentsPath, strings.Repeat("!", len(assign)), info.ModTime())
	if _, _, e = s.Load(); e == nil {
		t.Fatal("invalid JSON accepted")
	}
	replaceLoadFile(t, s.AssignmentsPath, assign, info.ModTime())
	_, a, e := s.Load()
	if e != nil || a.Output["pollSeconds"] != float64(300) {
		t.Fatalf("%v %v", a, e)
	}
}
func TestLoadConcurrent(t *testing.T) {
	s, orig, _ := loadFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, _, e := s.Load(); e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		content := orig
		if i%2 == 0 {
			content = strings.Replace(orig, "one", "two", 1)
		}
		tmp := filepath.Join(filepath.Dir(s.FleetPath), "replacement")
		if e := os.WriteFile(tmp, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(tmp, s.FleetPath); e != nil {
			t.Fatal(e)
		}
	}
	wg.Wait()
}

func TestLoadShippedConfiguration(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	s := &Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	doc, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	again, assignmentsAgain, err := s.Load()
	if err != nil || !reflect.DeepEqual(doc, again) || !reflect.DeepEqual(assignments, assignmentsAgain) {
		t.Fatalf("shipped configuration changed on cache hit: %v", err)
	}
}
