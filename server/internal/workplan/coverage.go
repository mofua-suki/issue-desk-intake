package workplan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var bullet = regexp.MustCompile(`^\s*(?:[-*+]|[0-9]+[.)])\s+`)

// Conditions extracts the acceptance list without interpreting quoted history or code.
func Conditions(body string) []string {
	out := []string{}
	inside := false
	fence := ""
	current := ""
	flush := func() {
		if strings.TrimSpace(current) != "" {
			out = append(out, strings.TrimSpace(current))
			current = ""
		}
	}
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
			mark := s[:3]
			if fence == "" {
				fence = mark
			} else if strings.HasPrefix(s, fence) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if strings.HasPrefix(s, "## ") {
			flush()
			inside = s == "## 受入条件" || s == "## 受入条件（テスト方法）"
			continue
		}
		if !inside {
			continue
		}
		if bullet.MatchString(line) {
			flush()
			current = bullet.ReplaceAllString(line, "")
		} else if current != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			current += " " + s
		} else {
			flush()
		}
	}
	flush()
	return out
}

// Condition is the body form of an acceptance condition: 「確認: <method> → 期待: <expected>」.
func (r Requirement) Condition() string { return "確認: " + r.Method + " → 期待: " + r.Expected }

func normalized(s string) string { return strings.Join(strings.Fields(s), " ") }
func (p *Plan) Covers(conditions []string) error {
	if len(conditions) == 0 {
		return errors.New("実行計画と照合する受入条件がありません")
	}
	covered := map[string]bool{}
	for _, r := range p.Requirements {
		covered[normalized(r.Criterion)] = true
		// The prompt asks every body condition in this form, whatever its evidence kind,
		// and executableAcceptanceAnswer rewrites fixed-test conditions with it, so it
		// names the same requirement as the criterion. Parse rejects an empty method or
		// expected result, so the form is never empty.
		covered[normalized(r.Condition())] = true
	}
	for _, condition := range conditions {
		if !covered[normalized(condition)] {
			return fmt.Errorf("実行分担がない受入条件: %s", condition)
		}
	}
	return nil
}
func (p *Plan) ValidateChildren(count int) error {
	for _, r := range p.Requirements {
		for _, index := range r.Children {
			if index > count {
				return fmt.Errorf("受入条件%sの対象の子%dがありません", r.ID, index)
			}
		}
	}
	for i := 1; i <= count; i++ {
		if len(p.ForChild(i).Requirements) == 0 {
			return fmt.Errorf("分割案%dの実行分担がありません", i)
		}
	}
	return nil
}
