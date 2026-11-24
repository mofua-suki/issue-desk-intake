// Package splitplan contains rules shared by split filing and pre-filing checks.
package splitplan

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"regexp"
	"strconv"
	"strings"
)

type Scope struct {
	Known bool
	Paths []any
}
type Proposal struct {
	Purpose     string
	Acceptance  []string
	WorkType    string
	ChangeScope Scope
	DependsOn   []any
}
type Reason string

const (
	DocsDependency           Reason = "docs_dependency"
	ImplementationDependency Reason = "implementation_dependency_with_contract"
)

type Correction struct {
	ProposalNumber    int
	RemovedDependency int
	Reason            Reason
	Before            []any
	After             []any
}

// ProposalKind returns an empty kind for incomplete scopes.
func ProposalKind(p Proposal) string {
	scope := p.ChangeScope
	if !scope.Known {
		return ""
	}
	paths := scope.Paths
	if len(paths) == 0 || len(paths) > 40 {
		return ""
	}
	docs, contract, uncertainMarkdown := true, true, false
	for _, item := range paths {
		path, ok := item.(string)
		if !ok || path == "*" {
			return ""
		}
		normalized, err := jobs.NormalizeScope([]string{path})
		if err != nil || len(normalized) != 1 {
			return ""
		}
		path = normalized[0]
		lower := strings.ToLower(path)
		if strings.HasSuffix(lower, ".md") && lower != "readme.md" && !strings.HasSuffix(lower, "/readme.md") {
			uncertainMarkdown = true
		}
		if !strings.HasSuffix(lower, ".md") && !strings.HasPrefix(path, "docs/") {
			docs = false
		}
		isContract := strings.HasPrefix(path, "api/") || strings.HasSuffix(path, ".schema.json") || strings.HasSuffix(path, ".proto")
		for _, part := range strings.Split(path, "/") {
			if part == "openapi" || part == "schemas" {
				isContract = true
			}
		}
		if !isContract {
			contract = false
		}
	}
	if uncertainMarkdown {
		return "implementation"
	}
	if p.WorkType == "docs" || docs {
		return "docs"
	}
	if contract {
		return "contract"
	}
	return "implementation"
}

var splitDependencyException = regexp.MustCompile(`依頼者の指定による依存: 分割案([0-9]+)`)

func Exceptions(p Proposal) map[string]bool {
	result := map[string]bool{}
	texts := append([]string{p.Purpose}, p.Acceptance...)
	for _, text := range texts {
		for _, match := range splitDependencyException.FindAllStringSubmatch(text, -1) {
			result[match[1]] = true
		}
	}
	return result
}

// CorrectDependencies returns independent dependency slices and one record per removed edge.
// Proposal numbers in protected are one-based.
func CorrectDependencies(proposals []Proposal, protected map[int]bool) ([][]any, []Correction) {
	result := make([][]any, len(proposals))
	var corrections []Correction
	kinds := make([]string, len(proposals))
	for i, p := range proposals {
		kinds[i] = ProposalKind(p)
	}
	for i, p := range proposals {
		result[i] = append([]any(nil), p.DependsOn...)
		if p.DependsOn != nil && result[i] == nil {
			result[i] = []any{}
		}
		if protected[i+1] || kinds[i] == "" {
			continue
		}
		hasContract := false
		for _, dep := range p.DependsOn {
			n := number(dep)
			if n > 0 && n <= len(kinds) && kinds[n-1] == "contract" {
				hasContract = true
			}
		}
		exceptions := Exceptions(p)
		kept := make([]any, 0, len(p.DependsOn))
		type removal struct {
			n      int
			reason Reason
		}
		var removed []removal
		for _, dep := range p.DependsOn {
			n := number(dep)
			if n < 1 || n > len(kinds) || kinds[n-1] == "" || exceptions[formatNumber(n)] {
				kept = append(kept, dep)
				continue
			}
			var reason Reason
			if kinds[i] == "docs" && kinds[n-1] != "contract" {
				reason = DocsDependency
			} else if kinds[i] == "implementation" && hasContract && kinds[n-1] == "implementation" {
				reason = ImplementationDependency
			}
			if reason == "" {
				kept = append(kept, dep)
			} else {
				removed = append(removed, removal{n, reason})
			}
		}
		if len(removed) == 0 {
			continue
		}
		result[i] = kept
		for _, r := range removed {
			corrections = append(corrections, Correction{i + 1, r.n, r.reason, append([]any{}, p.DependsOn...), append([]any{}, kept...)})
		}
	}
	return result, corrections
}
func number(v any) int          { return int(fleet.WholeNumber(v)) }
func formatNumber(n int) string { return strconv.Itoa(n) }

// FormatDependency describes retained values without turning unknown types into proposal zero.
func FormatDependency(v any) string {
	if n := number(v); n != 0 {
		return fmt.Sprintf("分割案%d", n)
	}
	return fmt.Sprintf("不明な依存(%v)", v)
}
