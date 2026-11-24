package startup

import (
	"bytes"
	"context"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConsultationEnvironmentUsesShippedAssignments(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	app := &App{Fleet: &fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}}
	hosts, err := app.consultationEnvironment(context.Background(), "mofua-suki/issue-desk-intake")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, h := range hosts {
		names = append(names, h.Name)
		for _, key := range []string{"gcc", "cgo", "race", "docker", "realDevice", "liveMeasurement", "network"} {
			if value, ok := h.Values[key]; !ok || value != nil {
				t.Fatalf("%s %s=%v", h.Name, key, value)
			}
		}
	}
	if !reflect.DeepEqual(names, []string{"gt12", "nab-9s"}) {
		t.Fatalf("担当=%v", names)
	}
}

func TestStartupFleetPlatform(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// 出荷のfleet.jsonは出荷の担当表そのものと組で検査する（Macのテスト担当を指すnativeTestHostは担当表のtestRepositoriesで照合し、
	// テスト専用のMacを外す shippedAssignments では照合できない）。
	openLifecycleApp(t, http.HandlerFunc(lifecycleGitea), func(config *Config) {
		assignments, err := os.ReadFile(filepath.Join(root, "assignments.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.AssignmentsPath, assignments, 0600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "fleet.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, platform := range []string{"windows", "darwin"} {
			invalid := bytes.Replace(data, []byte(`"platform": "linux"`), []byte(`"platform": "`+platform+`"`), 1)
			if err := os.WriteFile(config.FleetPath, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(config.File, config.ApplicationDir, ""); err == nil {
				t.Fatalf("%s: Linuxコンテナの理由がない: %v", platform, err)
			}
		}
		if err := os.WriteFile(config.FleetPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(config.File, config.ApplicationDir, "")
		if err != nil {
			t.Fatal(err)
		}
		loaded.AppPostgresDSN, loaded.ApplicationSchema = config.AppPostgresDSN, config.ApplicationSchema
		*config = *loaded
	})
}

func TestStartupWithoutTestPlacement(t *testing.T) {
	shipped := shippedAssignmentsPath(t)
	app := openLifecycleApp(t, lifecycleGitea, func(config *Config) { config.AssignmentsPath = shipped })
	if app.Engine == nil || app.Gitea == nil {
		t.Fatal("起動検査後にサービスが初期化されていません")
	}
	if _, err := app.Values(); err != nil {
		t.Fatalf("起動後の設定読込: %v", err)
	}
}
