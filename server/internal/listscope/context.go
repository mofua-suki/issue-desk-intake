// Package listscope carries a list request's range and shared read results.
package listscope

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

type key struct{}
type read struct {
	once  sync.Once
	value any
	err   error
}
type request struct {
	scope string
	mu    sync.Mutex
	reads map[string]*read
}

// WithScope starts a fresh request; cached reads never outlive its context.
func WithScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, key{}, &request{scope: scope, reads: map[string]*read{}})
}
func Active(ctx context.Context) bool {
	r, _ := ctx.Value(key{}).(*request)
	return r != nil && r.scope == "active"
}

// Read shares a read between concurrent row readers without copying its data.
func Read[T any](ctx context.Context, name string, load func() (T, error)) (T, error) {
	r, _ := ctx.Value(key{}).(*request)
	if r == nil {
		return load()
	}
	r.mu.Lock()
	v := r.reads[name]
	if v == nil {
		v = &read{}
		r.reads[name] = v
	}
	r.mu.Unlock()
	v.once.Do(func() { v.value, v.err = load() })
	value, _ := v.value.(T)
	return value, v.err
}

type protectionKey struct{}
type Protection struct {
	All    bool
	Drafts map[string]bool
	Issues map[string]bool
}

func IssueKey(project string, number any) string {
	value := fmt.Sprint(number)
	switch n := number.(type) {
	case float64:
		value = strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		value = strconv.FormatFloat(float64(n), 'f', -1, 32)
	}
	return project + "#" + value
}
func WithProtection(ctx context.Context, p Protection) context.Context {
	return context.WithValue(ctx, protectionKey{}, p)
}
func Protected(ctx context.Context, project string, number any, draft string) bool {
	p, _ := ctx.Value(protectionKey{}).(Protection)
	return p.All || p.Issues[IssueKey(project, number)] || draft != "" && p.Drafts[draft]
}

type omittedKey struct{}

// WithOmitted collects row omissions in the single Issue reader. Read the map
// only after that reader finishes; it contains no Gitea or stored-record changes.
func WithOmitted(ctx context.Context, omitted map[string]bool) context.Context {
	return context.WithValue(ctx, omittedKey{}, omitted)
}
func Omit(ctx context.Context, project string, number any) {
	if m, ok := ctx.Value(omittedKey{}).(map[string]bool); ok {
		m[IssueKey(project, number)] = true
	}
}

type resolverKey struct{}

func WithProtectionResolver(ctx context.Context, resolve func([]map[string]any) Protection) context.Context {
	return context.WithValue(ctx, resolverKey{}, resolve)
}
func ResolveProtection(ctx context.Context, issues []map[string]any) context.Context {
	if resolve, ok := ctx.Value(resolverKey{}).(func([]map[string]any) Protection); ok {
		return WithProtection(ctx, resolve(issues))
	}
	return ctx
}
