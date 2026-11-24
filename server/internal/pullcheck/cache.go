// Package pullcheck shares a PR response only within one filing validation boundary.
package pullcheck

import "context"

type key struct{}
type entry struct {
	repo, link string
	value      map[string]any
}

func Boundary(ctx context.Context) context.Context { return context.WithValue(ctx, key{}, &entry{}) }
func Get(ctx context.Context, repo, link string) map[string]any {
	e, _ := ctx.Value(key{}).(*entry)
	if e != nil && e.repo == repo && e.link == link {
		return e.value
	}
	return nil
}
func Put(ctx context.Context, repo, link string, value map[string]any) {
	if e, _ := ctx.Value(key{}).(*entry); e != nil {
		e.repo, e.link, e.value = repo, link, value
	}
}
