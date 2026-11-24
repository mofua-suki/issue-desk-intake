// Package screenonly guards against new approvals or operations that only the
// intake screen can send. The user often cannot open the screen (it is heavy and
// unreachable away from home), so every approval must also be possible from
// the user's chat: a PC token with 運用の代行 relaying the user's own statement
// (auth.UserApproval). See chat-instructions.md and AGENTS.md.
package screenonly

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// marks are heuristics for a screen-only check. Finding one is not a verdict:
// either accept the chat route too (auth.UserApproval) or, when the check is not
// a user approval (reads for the screen, CSRF, the token management that grants
// the AI its own rights), record it in allow.txt with the reason.
var marks = map[string]func(line string) int{
	// 画面を含み PC を許さない Policy の値（Screen: true, CLI: … も数える）。
	"screen-policy": func(line string) int {
		n := 0
		for _, m := range policyLiteral.FindAllStringSubmatch(line, -1) {
			if screenTrue.MatchString(m[1]) && !pcTrue.MatchString(m[1]) {
				n++
			}
		}
		return n
	},
	"screen-kind":    count(`Kind\s*!=\s*auth\.Screen\b`),
	"screen-source":  count(`[!=]=\s*"screen-session"`),
	"screen-message": count(`\(403, "[^"]*画面`),
	// 経路ごとに PC を外す書き方（policy.PC = false）。
	"pc-off": count(`\.PC\s*=\s*false\b`),
}

var (
	policyLiteral = regexp.MustCompile(`Policy\{([^}]*)\}`)
	screenTrue    = regexp.MustCompile(`\bScreen:\s*true\b`)
	pcTrue        = regexp.MustCompile(`\bPC:\s*true\b`)
)

func count(pattern string) func(string) int {
	re := regexp.MustCompile(pattern)
	return func(line string) int { return len(re.FindAllStringIndex(line, -1)) }
}

type allowance struct {
	max    int
	reason string
}

func scan(root string) (map[string]int, error) {
	out := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			for name, pattern := range marks {
				out["server/"+filepath.ToSlash(rel)+"|"+name] += pattern(line)
			}
		}
		return nil
	})
	for key, count := range out {
		if count == 0 {
			delete(out, key)
		}
	}
	return out, err
}

func readAllow(path string) (map[string]allowance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]allowance{}
	lines := bufio.NewScanner(f)
	for n := 1; lines.Scan(); n++ {
		line := lines.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, ok := strings.Cut(line, "\t")
		parts := strings.Split(key, "|")
		if !ok || strings.TrimSpace(reason) == "" || len(parts) != 3 || marks[parts[1]] == nil {
			return nil, fmt.Errorf("allow.txt:%d は `ファイル|目印|件数<TAB>理由` で書く", n)
		}
		max, err := strconv.Atoi(parts[2])
		if err != nil || max < 1 {
			return nil, fmt.Errorf("allow.txt:%d の件数が不正です", n)
		}
		out[parts[0]+"|"+parts[1]] = allowance{max, reason}
	}
	return out, lines.Err()
}

// 画面からだけ受け付ける判定は、許可一覧（理由付き）にあるものだけ。新しく足したら、チャットの承認の経路を付けるか、
// 利用者の承認ではない理由を書いて allow.txt に足す。判定を消したら許可項目も消す（件数は減ってよい）。
func TestNoNewScreenOnlyChecks(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	found, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := readAllow("allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for key, count := range found {
		if a, ok := allowed[key]; !ok || count > a.max {
			problems = append(problems, fmt.Sprintf("%s: %d件（許可 %d件）。画面だけの判定を足さず auth.UserApproval でチャットの承認も受け付けるか、利用者の承認でない理由を allow.txt に書く", key, count, a.max))
		}
	}
	for key := range allowed {
		if found[key] == 0 {
			problems = append(problems, key+": 検出されなくなったので allow.txt から消す")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func TestScanFindsScreenOnlyMarks(t *testing.T) {
	dir := t.TempDir()
	src := "package x\nfunc f() {\n\tif p.Kind != auth.Screen {\n\t\treturn fail(403, \"受付画面から行ってください。\")\n\t}\n\t_ = auth.Policy{Screen: true, CLI: true}\n\t_ = auth.Policy{Screen: true, PC: true}\n\t_ = kind == \"screen-session\"\n\tpolicy.PC = false\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name := range marks {
		if found["server/x.go|"+name] != 1 {
			t.Fatalf("%s を検出しない: %v", name, found)
		}
	}
	if len(found) != len(marks) {
		t.Fatalf("テストのファイルまで数えた: %v", found)
	}
}
