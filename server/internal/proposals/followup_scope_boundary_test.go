package proposals

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

func TestFollowupScopeDisplayAndParserBoundaries(t *testing.T) {
	module, err := filepath.Abs(filepath.Join("..", "..", "..", "shared", "src", "change-scope.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{3, 40} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, api, restart := adoptFixture(t)
			p := v3()
			paths := make([]string, 0, count)
			explicit := make([]any, 0, count)
			api.files = map[string][]Object{}
			for i := 0; i < count; i++ {
				path := fmt.Sprintf("src/file%03d.go", i)
				paths = append(paths, path)
				explicit = append(explicit, path)
				route := fmt.Sprintf("/repos/owner/repo/pulls/2/files?limit=50&page=%d", i/50+1)
				api.files[route] = append(api.files[route], Object{"filename": path})
			}
			p["changeScope"] = Object{"paths": explicit}
			api.comments[0] = proposalComment(p, 10)
			s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
			mustScan(t, s)
			restart()
			prepared, err := s.PrepareFollowup(context.Background(), savedDraft(t, s), Object{"provider": "codex", "model": "fixture", "effort": "high"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			parsed := filing.ParseScope(prepared.Body)
			if parsed.State != "declared" || !reflect.DeepEqual(parsed.Paths, paths) || !reflect.DeepEqual(prepared.Job.Scope, paths) {
				t.Fatalf("scope=%+v job=%v", parsed, prepared.Job.Scope)
			}
			for _, path := range paths {
				visible := strings.Contains(prepared.Body, "- `"+path+"`\n")
				if visible != (count <= 40) {
					t.Fatalf("count=%d path=%s visible=%v", count, path, visible)
				}
			}
			if len(utf16.Encode([]rune(prepared.Body))) > 60000 {
				t.Fatal("本文が既定の上限を超えました")
			}
			if count > 40 && !strings.Contains(prepared.Body, "- 変更対象は直後のJSONマーカーを参照してください。\n") {
				t.Fatal("案内文がありません")
			}
			js := exec.Command("node", "--input-type=module", "-e", `import {pathToFileURL} from 'node:url'; import {readFileSync} from 'node:fs'; const m=await import(pathToFileURL(process.argv[1])); process.stdout.write(m.parseScope(readFileSync(0,'utf8')).state);`, module)
			js.Stdin = strings.NewReader(prepared.Body)
			output, err := js.CombinedOutput()
			want := "declared"
			if count > 40 {
				want = "unknown"
			}
			if err != nil || string(output) != want {
				t.Fatalf("JS state=%s want=%s err=%v", output, want, err)
			}
		})
	}
}
