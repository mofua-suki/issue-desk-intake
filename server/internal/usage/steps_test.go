package usage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Inspect the shipped recording calls, so a new literal step cannot silently miss the mapping.
func TestRecordedIntakeStepsAreMapped(t *testing.T) {
	files, err := filepath.Glob("../consultationai/*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range files {
		if len(name) >= 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := usageAliases(f)
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || len(c.Args) < 5 {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "call" {
				return true
			}
			step, err := recordedStep(c.Args[4], aliases)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return true
			}
			if step == TierClassificationStep || step == FilingScopeStep {
				return true
			}
			seen[step] = true
			if !IsIntakeStep(step) {
				t.Errorf("%s: unmapped recording step %q", name, step)
			}
			return true
		})
	}
	// Filing uses the Answer interface rather than consultationai.call.
	f, err := parser.ParseFile(token.NewFileSet(), "../filingcheck/check.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		k, ok := kv.Key.(*ast.BasicLit)
		if !ok || k.Value != `"step"` {
			return true
		}
		step, err := recordedStep(kv.Value, usageAliases(f))
		if err != nil {
			t.Errorf("filing step: %v", err)
			return true
		}
		seen[step] = true
		if !IsIntakeStep(step) {
			t.Errorf("unmapped filing step %q", step)
		}
		return true
	})
	for _, step := range []string{DraftStep, ProxyConfirmationStep, FilingCheckStep} {
		if !seen[step] {
			t.Errorf("recording step %q not found", step)
		}
	}
}

func TestIntakeStepValidation(t *testing.T) {
	for _, step := range []string{ConsultationStep, ProxyStep, FilingStep, DraftStep, ProxyConfirmationStep, FilingCheckStep} {
		row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas", "provider": "codex", "step": step, "callTrigger": "initial"}
		if !ValidUsage(row) {
			t.Errorf("rejected %s", step)
		}
	}
}

// Answer can record its explicit step as a usage label. Inspect production callers,
// including those outside consultationai, so new labels require a deliberate decision.
func TestAnswerStepsAreMappedOrExcluded(t *testing.T) {
	seen := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Answer" || len(call.Args) < 2 {
				return true
			}
			if !isAnswerReceiver(file, call) {
				if arg, ok := call.Args[1].(*ast.CompositeLit); ok {
					if _, ok := arg.Type.(*ast.MapType); ok {
						t.Errorf("%s: unregistered Answer receiver with literal map; inspect its step", fset.Position(call.Pos()))
					}
				}
				return true
			}
			arg, ok := call.Args[1].(*ast.CompositeLit)
			if !ok {
				t.Errorf("%s: Answer request is not a literal map; inspect its step", fset.Position(call.Pos()))
				return true
			}
			for _, elt := range arg.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING || key.Value != `"step"` {
					continue
				}
				step, err := recordedStep(kv.Value, usageAliases(file))
				if err != nil {
					t.Errorf("%s: Answer step: %v", fset.Position(kv.Pos()), err)
					continue
				}
				seen[step] = true
				if !excludedAnswerSteps[step] && !IsIntakeStep(step) {
					t.Errorf("%s: unmapped Answer step %q", fset.Position(kv.Pos()), step)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{FilingCheckStep} {
		if !seen[step] {
			t.Errorf("Answer step %q not found", step)
		}
	}
}

// Excluded production steps must remain valid without a consultation call trigger.
func TestExcludedStepValidation(t *testing.T) {
	for _, step := range []string{ReviewNotesStep, TierClassificationStep, FilingScopeStep} {
		t.Run(step, func(t *testing.T) {
			row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas", "provider": "codex", "step": step}
			if !ValidUsage(row) {
				t.Fatal("step without trigger rejected")
			}
			row["callTrigger"] = "initial"
			if ValidUsage(row) {
				t.Fatal("excluded step accepted with trigger")
			}
		})
	}
}

// Only explicit literals and known constants from the actual usage import are accepted.
func usageAliases(f *ast.File) map[string]bool {
	aliases := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "issue-desk-intake/server/internal/usage" {
			continue
		}
		name := "usage"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "." && name != "_" {
			aliases[name] = true
		}
	}
	return aliases
}

func recordedStep(expr ast.Expr, aliases map[string]bool) (string, error) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		return strconv.Unquote(lit.Value)
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && aliases[pkg.Name] {
			switch sel.Sel.Name {
			case "ProxyConfirmationStep":
				return ProxyConfirmationStep, nil
			case "FilingCheckStep":
				return FilingCheckStep, nil
			case "DraftStep":
				return DraftStep, nil
			case "TierClassificationStep":
				return TierClassificationStep, nil
			case "FilingScopeStep":
				return FilingScopeStep, nil
			}
		}
	}
	return "", fmt.Errorf("recording step must be a string literal or known usage constant")
}

func TestRecordedStepRejectsUnknownExpressions(t *testing.T) {
	for _, expr := range []string{"localStep", "other.DraftStep", "u.UnknownStep"} {
		e, err := parser.ParseExpr(expr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recordedStep(e, map[string]bool{"u": true}); err == nil {
			t.Errorf("accepted %s", expr)
		}
	}
	e, err := parser.ParseExpr("u.DraftStep")
	if err != nil {
		t.Fatal(err)
	}
	if step, err := recordedStep(e, map[string]bool{"u": true}); err != nil || step != DraftStep {
		t.Errorf("aliased constant: %q %v", step, err)
	}
}

// Restrict the dynamic entry point to the two deliberate production paths.
func TestCallDynamicCallers(t *testing.T) {
	files, err := filepath.Glob("../consultationai/*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"answer.go:Answer": false, "call.go:call": false}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range dynamicReferences(f) {
			key := filepath.Base(name) + ":" + ref.function
			if _, ok := allowed[key]; !ok {
				t.Errorf("%s: unexpected callDynamic reference in %s", fset.Position(ref.pos), key)
			} else {
				allowed[key] = true
			}
		}
	}
	for key, found := range allowed {
		if !found {
			t.Errorf("expected callDynamic caller missing: %s", key)
		}
	}
}

// dynamicReferences scans the entire file, including package initializers.
func dynamicReferences(f *ast.File) []struct {
	function string
	pos      token.Pos
} {
	var refs []struct {
		function string
		pos      token.Pos
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "callDynamic" {
			return true
		}
		owner := "<package>"
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Body.Pos() <= sel.Pos() && sel.End() <= fn.Body.End() {
				owner = fn.Name.Name
				break
			}
		}
		refs = append(refs, struct {
			function string
			pos      token.Pos
		}{owner, sel.Pos()})
		return true
	})
	return refs
}
