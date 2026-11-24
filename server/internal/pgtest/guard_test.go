package pgtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pgtest is test support: only _test.go files may import it, so the helper (and
// the appdb package it reuses) never reaches a production binary.
func TestOnlyTestFilesImportPgtest(t *testing.T) {
	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(slash, "/internal/pgtest/") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		if strings.Contains(string(raw), `internal/pgtest"`) {
			t.Errorf("本番コード %s が試験支援の pgtest を取り込んでいます", slash)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("検査した本番ファイルが少なすぎます: %d", checked)
	}
}
