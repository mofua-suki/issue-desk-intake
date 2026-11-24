package usage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// These Answer labels do not represent intake consultation metrics.
var excludedAnswerSteps = map[string]bool{"close": true, "intake": true, "review": true, "project": true, "有事の期限切れの対処": true}

// The callbacks are wired to consultationai.Service.Answer at startup. Match
// the enclosing receiver type and selector path, not the method name alone.
func isAnswerReceiver(f *ast.File, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	var receiver string
	switch x := sel.X.(type) {
	case *ast.Ident:
		receiver = x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			receiver = id.Name + "." + x.Sel.Name
		}
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || call.Pos() < fn.Body.Pos() || call.End() > fn.Body.End() || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		field := fn.Recv.List[0]
		if len(field.Names) != 1 {
			continue
		}
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		typ, ok := star.X.(*ast.Ident)
		if !ok {
			continue
		}
		base := field.Names[0].Name
		switch f.Name.Name + ":" + typ.Name {
		case "consultationai:Service":
			return receiver == base
		case "filingcheck:Service":
			return receiver == base+".options"
		case "issueapi:Service", "projects:Service":
			return receiver == base+".o"
		}
	}
	// startup uses a locally scoped application, not a method receiver.
	if f.Name.Name == "startup" && receiver == "a.AI" {
		return true
	}
	return false
}

func TestAnswerReceiverIgnoresUnrelatedTypes(t *testing.T) {
	src := `package consultationai
 type Service struct{}; type Other struct{}
 func (s *Service) f() { s.Answer(nil, nil) }
 func (s *Other) f() { s.Answer(nil, nil) }`
	f, err := parser.ParseFile(token.NewFileSet(), "example.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []bool
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			got = append(got, isAnswerReceiver(f, c))
		}
		return true
	})
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("receiver matches: %v", got)
	}
}

// Inspect every assignment to the values passed as the recording label. A new
// dynamic expression requires review rather than silently escaping the check.
func answerDefaultLabels(f *ast.File) ([]string, error) {
	var values []string
	var problem error
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Answer" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			a, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range a.Lhs {
				target := ""
				switch x := lhs.(type) {
				case *ast.Ident:
					target = x.Name
				case *ast.IndexExpr:
					id, ok := x.X.(*ast.Ident)
					if !ok || id.Name != "request" {
						break
					}
					key, ok := x.Index.(*ast.BasicLit)
					if ok && key.Value == `"step"` {
						target = "request.step"
					}
				}
				if target != "request.step" && target != "step" && target != "label" {
					continue
				}
				if i >= len(a.Rhs) {
					problem = fmt.Errorf("unsupported %s assignment", target)
					continue
				}
				rhs := a.Rhs[i]
				if lit, ok := rhs.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						problem = err
					} else {
						values = append(values, v)
					}
					continue
				}
				if target == "step" {
					if c, ok := rhs.(*ast.CallExpr); ok {
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Text" && isIdent(sel.X, "fleet") && len(c.Args) == 1 {
							if idx, ok := c.Args[0].(*ast.IndexExpr); ok {
								if id, ok := idx.X.(*ast.Ident); ok && id.Name == "request" {
									if key, ok := idx.Index.(*ast.BasicLit); ok && key.Value == `"step"` {
										continue
									}
								}
							}
						}
					}
				}
				if target == "label" {
					if id, ok := rhs.(*ast.Ident); ok && id.Name == "step" {
						continue
					}
				}
				problem = fmt.Errorf("unsupported %s expression at %d", target, rhs.Pos())
			}
			return true
		})
		return false
	})
	return values, problem
}

func TestAnswerDefaultLabelsAreMappedOrExcluded(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../consultationai/answer.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	values, err := answerDefaultLabels(f)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range values {
		seen[v] = true
		if !excludedAnswerSteps[v] && !IsIntakeStep(v) {
			t.Errorf("unclassified Answer label %q", v)
		}
	}
	for v := range excludedAnswerSteps {
		if !seen[v] {
			t.Errorf("expected Answer default %q not found", v)
		}
	}
}

func TestAnswerDefaultLabelsRejectUnknownExpressions(t *testing.T) {
	for _, src := range []string{
		`package p; func (s *Service) Answer() { request["step"] = computed() }`,
		`package p; func (s *Service) Answer() { label = computed() }`,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "example.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := answerDefaultLabels(f); err == nil {
			t.Errorf("accepted dynamic assignment: %s", src)
		}
	}
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}
