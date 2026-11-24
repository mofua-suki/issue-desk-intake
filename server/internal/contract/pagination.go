package contract

import (
	"net/url"
	"strconv"
)

// Pagination is the validated, one-based page request.
type Pagination struct{ Page, Limit int }

func ParsePagination(query url.Values) (Pagination, *Problem) {
	p := Pagination{Page: 1, Limit: 100}
	for _, field := range []struct {
		name string
		dst  *int
		max  int
	}{{"page", &p.Page, 0}, {"limit", &p.Limit, 200}} {
		values, exists := query[field.name]
		if !exists {
			continue
		}
		if len(values) != 1 || values[0] == "" {
			return p, NewProblem(400, "ページ指定が不正です。", "")
		}
		for _, c := range values[0] {
			if c < '0' || c > '9' {
				return p, NewProblem(400, "ページ指定が不正です。", "")
			}
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || (field.max > 0 && n > field.max) {
			return p, NewProblem(400, "ページ指定が不正です。", "")
		}
		*field.dst = n
	}
	return p, nil
}

type PageResult[T any] struct {
	Items   []T  `json:"items"`
	Total   int  `json:"total"`
	Page    int  `json:"page"`
	Limit   int  `json:"limit"`
	HasMore bool `json:"hasMore"`
}

func Paginate[T any](items []T, p Pagination) PageResult[T] {
	total := len(items)
	start := total
	// Avoid overflow on extremely large page numbers.
	if p.Page-1 <= total/p.Limit {
		start = (p.Page - 1) * p.Limit
		if start > total {
			start = total
		}
	}
	end := start + min(p.Limit, total-start)
	return PageResult[T]{Items: items[start:end], Total: total, Page: p.Page, Limit: p.Limit, HasMore: end < total}
}
