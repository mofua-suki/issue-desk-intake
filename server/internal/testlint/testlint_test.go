package testlint

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type finding struct{ file, test, mark string }

func (f finding) key() string { return f.file + "|" + f.test + "|" + f.mark }
func (f finding) String() string {
	return f.key() + ": 挙動を検証するテストに直すか、理由を付けて許可一覧に足す"
}

// suppliedEnv reports the environment variables the fixed verification sets:
// assignments in the verification scripts and self re-exec helpers in Go tests.
func suppliedEnv(scripts []string, goTests []string) func(string) bool {
	return func(name string) bool {
		assign := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*([:=].)`)
		for _, text := range scripts {
			for _, m := range assign.FindAllStringSubmatchIndex(text, -1) {
				op := text[m[2]:m[3]]
				if !strings.HasSuffix(text[:m[0]], "${") {
					if op[1] != '=' { // NAME == x は比較
						return true
					}
					continue
				}
				// ${NAME:-x}・${NAME-x} は読み取り。${NAME:=x}・${NAME=x} だけが未設定時に代入する。
				if op == ":=" || op[0] == '=' {
					return true
				}
			}
		}
		for _, text := range goTests {
			if strings.Contains(text, `"`+name+`=`) {
				return true
			}
		}
		return false
	}
}

// repositorySupplied reads scripts/ (excluding tests) and every Go test under server/.
func repositorySupplied(repo string) (func(string) bool, error) {
	var scripts, goTests []string
	err := filepath.WalkDir(filepath.Join(repo, "scripts"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(d.Name(), ".test.") {
			return err
		}
		b, err := os.ReadFile(path)
		scripts = append(scripts, string(b))
		return err
	})
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(filepath.Join(repo, "server"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		goTests = append(goTests, string(b))
		return err
	})
	return suppliedEnv(scripts, goTests), err
}

// buildConstraint returns the file's //go:build expression, or nil.
func buildConstraint(file *ast.File) constraint.Expr {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if constraint.IsGoBuild(c.Text) {
				if expr, err := constraint.Parse(c.Text); err == nil {
					return expr
				}
			}
		}
	}
	return nil
}

// defaultBuildTag reports the tags `go test` satisfies without -tags (scripts/verify.sh passes none).
func defaultBuildTag(tag string) bool {
	switch tag {
	case runtime.GOOS, runtime.GOARCH, runtime.Compiler, "cgo":
		return true
	case "unix":
		return runtime.GOOS != "windows" && runtime.GOOS != "plan9" && runtime.GOOS != "js" && runtime.GOOS != "wasip1"
	}
	return strings.HasPrefix(tag, "go1.")
}

// scan reads each Go test once. Findings are heuristics, not quality verdicts.
// supplied reports environment variables the fixed verification sets.
func scan(root string, supplied func(string) bool) ([]finding, error) {
	var out []finding
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "fixtures" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = "server/" + filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, path, data, parser.ParseComments)
		if err != nil {
			return err
		}
		add := func(name, mark string) { out = append(out, finding{rel, name, mark}) }
		if expr := buildConstraint(file); expr != nil && !expr.Eval(defaultBuildTag) {
			add("(ファイル単位: テスト名を特定不可)", "g:固定検証が付けないビルドタグ")
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "go/parser" || p == "go/ast" || p == "go/format" {
				add("(ファイル単位: テスト名を特定不可)", "b:"+p)
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !(strings.HasPrefix(fn.Name.Name, "Test") || strings.HasPrefix(fn.Name.Name, "Benchmark")) {
				continue
			}
			var read, compare, product, env, skip, measure, legacy bool
			var envNames []string
			for _, stmt := range fn.Body.List {
				if expr, ok := stmt.(*ast.ExprStmt); ok {
					if call, ok := expr.X.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Skip" || sel.Sel.Name == "Skipf" || sel.Sel.Name == "SkipNow") {
							add(fn.Name.Name, "e:常に省略")
						}
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						s, _ := strconv.Unquote(x.Value)
						lower := strings.ToLower(s)

						for _, ext := range []string{".go", ".md", ".sh", "openapi", "readme"} {
							if strings.Contains(lower, ext) && !strings.Contains(lower, "testdata") && !strings.Contains(lower, "fixtures") {
								product = true
							}
						}
						if strings.Contains(lower, "bench") || strings.Contains(lower, "measure") || strings.Contains(lower, "timing") {
							measure = true
						}
					}
				case *ast.Ident:
					s := strings.ToLower(x.Name)

					if strings.Contains(s, "measure") || strings.Contains(s, "benchmark") || strings.Contains(s, "timing") {
						measure = true
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") && len(x.Args) == 1 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if name, err := strconv.Unquote(lit.Value); err == nil {
								envNames = append(envNames, name)
							}
						}
					}
					if id, ok := x.Fun.(*ast.Ident); ok && strings.HasPrefix(strings.ToLower(id.Name), "legacy") {
						legacy = true
					}
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && strings.Contains(strings.ToLower(sel.Sel.Name), "legacy") {
						legacy = true
					}
				case *ast.SelectorExpr:
					switch x.Sel.Name {
					case "ReadFile", "Open", "ReadAll":
						read = true
					case "Contains", "ContainsAny", "Equal", "Match", "MatchString", "MustCompile", "Compile", "Index", "HasPrefix", "HasSuffix":
						compare = true
					case "Now", "Since":
						if id, ok := x.X.(*ast.Ident); ok && id.Name == "time" {
							measure = true
						}
					case "Getenv", "LookupEnv":
						env = true
					case "Skip", "Skipf", "SkipNow":
						skip = true
					}
				case *ast.BinaryExpr:
					if x.Op == token.EQL || x.Op == token.NEQ {
						compare = true
					}
				}
				return true
			})
			if read && compare && product {
				add(fn.Name.Name, "a:製品ファイルの文字列照合")
			}
			name := strings.ToLower(fn.Name.Name)
			if env && skip && (measure || strings.Contains(name, "measure") || strings.Contains(name, "timing") || strings.Contains(name, "benchmark") || strings.Contains(name, "performance")) {
				add(fn.Name.Name, "c:環境変数で省略される計測")
			} else if skip {
				for _, envName := range envNames {
					if !supplied(envName) {
						add(fn.Name.Name, "f:固定検証が設定しない環境変数で省略")
						break
					}
				}
			}
			if legacy && compare {
				add(fn.Name.Name, "d:旧実装との比較")
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, err
}

func check(findings []finding, allow string) []string {
	entries := map[string]string{}
	var problems []string
	s := bufio.NewScanner(strings.NewReader(allow))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			problems = append(problems, "不正な許可項目: "+line)
			continue
		}
		if _, ok := entries[parts[0]]; ok {
			problems = append(problems, "重複した許可項目: "+parts[0])
		}
		entries[parts[0]] = parts[1]
	}
	if err := s.Err(); err != nil {
		problems = append(problems, err.Error())
	}
	seen := map[string]bool{}
	for _, f := range findings {
		k := f.key()
		seen[k] = true
		if _, ok := entries[k]; !ok {
			problems = append(problems, f.String())
		}
	}
	for k := range entries {
		if !seen[k] {
			problems = append(problems, "検出されない許可項目: "+k)
		}
	}
	sort.Strings(problems)
	return problems
}
func TestRepository(t *testing.T) {
	supplied, err := repositorySupplied("../../..")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := scan("../..", supplied)
	if err != nil {
		t.Fatal(err)
	}
	allow, err := os.ReadFile("allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range check(findings, string(allow)) {
		t.Error(p)
	}
	t.Logf("目印 %d 件", len(findings))
}
func TestDetectionAndAllowlist(t *testing.T) {
	root := t.TempDir()
	src := `package sample
import ("go/parser"; "os"; "strings")
func TestRead(t *testing.T) { b,_:=os.ReadFile("README.md"); if strings.Contains(string(b), "x") {} }
func TestFixture(t *testing.T) { b,_:=os.ReadFile("testdata/a.go"); if strings.Contains(string(b), "x") {} }
func TestMeasure(t *testing.T) { if os.Getenv("RUN")=="" { t.Skip("off") }; measure() }
func TestCompare(t *testing.T) { if legacyResult()!=result() {} }
func TestAlways(t *testing.T) { t.Skip("off"); run() }
func TestOptional(t *testing.T) { if os.Getenv("OPTIONAL")=="" { t.Skip("off") }; run() }
func TestSupplied(t *testing.T) { if os.Getenv("SUPPLIED")=="" { t.Skip("off") }; run() }
func TestPlatform(t *testing.T) { if runtime.GOOS!="linux" { t.Skip("linux") }; run() }
`
	if err := os.WriteFile(filepath.Join(root, "sample_test.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"live_test.go":     "//go:build live_eval\n\npackage sample\nfunc TestLive(t *testing.T) {}\n",
		"platform_test.go": "//go:build " + runtime.GOOS + " && !live_eval\n\npackage sample\nfunc TestHere(t *testing.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "fixtures"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixtures", "ignored_test.go"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	found, err := scan(root, func(name string) bool { return name == "SUPPLIED" })
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 7 {
		t.Fatalf("findings: %v", found)
	}
	joined := fmt.Sprint(found)
	for _, expected := range []string{"TestRead|a:", "テスト名を特定不可)|b:go/parser", "TestMeasure|c:", "TestCompare|d:", "TestAlways|e:", "TestOptional|f:", "live_test.go|(ファイル単位: テスト名を特定不可)|g:"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %s in %s", expected, joined)
		}
	}
	for _, absent := range []string{"TestFixture", "TestSupplied", "TestPlatform", "TestMeasure|f:", "platform_test.go"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("%s must not be reported: %s", absent, joined)
		}
	}
	var allow strings.Builder
	for _, f := range found {
		fmt.Fprintf(&allow, "%s\t理由\n", f.key())
	}
	if p := check(found, allow.String()); len(p) != 0 {
		t.Fatal(p)
	}
	p := check(found[:len(found)-1], allow.String())
	if len(p) != 1 || !strings.Contains(p[0], "検出されない") {
		t.Fatal(p)
	}
	p = check(found, "")
	if len(p) != 7 || !strings.Contains(strings.Join(p, "\n"), "挙動を検証するテストに直すか") {
		t.Fatal(p)
	}
}

func TestSuppliedEnv(t *testing.T) {
	supplied := suppliedEnv(
		[]string{"run({ ...env, DSN: dsn.href })", "export SHELL_VAR=1", "if (env.READ_ONLY) {}", "x = COMPARED == 1", `dir=${SHELL_READ:-/tmp}`, `: "${SHELL_DEFAULT:=1}"`},
		[]string{`cmd.Env = append(os.Environ(), "HELPER=1")`},
	)
	for name, want := range map[string]bool{"DSN": true, "SHELL_VAR": true, "HELPER": true, "READ_ONLY": false, "COMPARED": false, "SHELL_READ": false, "SHELL_DEFAULT": true, "MISSING": false} {
		if got := supplied(name); got != want {
			t.Errorf("%s: supplied=%v, want %v", name, got, want)
		}
	}
}
