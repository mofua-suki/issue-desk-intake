package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The intake binary must not link a SQLite driver: production runs on PostgreSQL only.
func TestIntakeBinaryHasNoSQLiteDriver(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go コマンドが見つかりません: %v", err)
	}
	cmd := exec.Command(goTool, "list", "-deps", ".")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("依存を列挙できません: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(strings.ToLower(dep), "sqlite") || strings.HasSuffix(dep, "/internal/appdb") || strings.HasSuffix(dep, "/internal/pgtest") {
			t.Errorf("受付の本体が SQLite 関連を取り込んでいます: %s", dep)
		}
	}
}
