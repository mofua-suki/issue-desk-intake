package issueapi

// 本文の依存マーカーを解析し、依存先のIssueと共通番号を返す。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Service) dependencies(ctx context.Context, repo, body string) []any {
	out := []any{}
	prefix := "<!-- issue-desk-dependencies:"
	at := strings.LastIndex(body, prefix)
	if at < 0 {
		return out
	}
	tail := body[at+len(prefix):]
	end := strings.Index(tail, " -->")
	if end < 0 {
		return []any{Object{"invalid": "依存マーカーを読めません"}}
	}
	var items []any
	if json.Unmarshal([]byte(tail[:end]), &items) != nil {
		return []any{Object{"invalid": "依存マーカーを読めません"}}
	}
	for _, v := range items {
		target := repo
		n, ok := number(v)
		if text, yes := v.(string); yes {
			parts := strings.Split(text, "#")
			if len(parts) == 2 {
				target = parts[0]
				n, ok = number(parts[1])
			}
		}
		if !ok || !repoPattern.MatchString(target) {
			out = append(out, Object{"invalid": fmt.Sprint(v)})
			continue
		}
		m := Object{"number": n, "desk": nil}
		if target != repo {
			m["project"] = target
		}
		if desk, found, e := s.o.Numbers.Number(ctx, target, n); e == nil && found {
			m["desk"] = desk
		}
		out = append(out, m)
	}
	return out
}
