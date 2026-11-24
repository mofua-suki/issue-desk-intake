package startup

import (
	"os"
	"path/filepath"
	"testing"
)

// Stage the actual runtime assets shipped by deploy/nas/Dockerfile. Keep the
// fixture's config/state isolated, without inventing a replacement API contract
// or weakening OpenApp's missing-asset checks.
func stageApplicationAssets(t *testing.T, destination string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	api, err := os.ReadFile(filepath.Join(root, "api", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(destination, "api"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(destination, "api", "openapi.json"), api, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.CopyFS(filepath.Join(destination, "server", "schemas"), os.DirFS(filepath.Join(root, "server", "schemas"))); err != nil {
		t.Fatal(err)
	}
}
