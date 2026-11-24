package screenapi

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// UnreadProject is a candidate repository whose Issue rows are missing from a
// list snapshot because it could not be read.
type UnreadProject struct {
	Project string `json:"project"`
	Reason  string `json:"reason"`
}

type unreadKey struct{}
type unreadCollector struct {
	mu    sync.Mutex
	items map[string]string
}

// ReportUnreadProjects lets the Issue list reader report candidates it could
// not read during one snapshot. Outside a snapshot it does nothing.
func ReportUnreadProjects(ctx context.Context, items []UnreadProject) {
	c, ok := ctx.Value(unreadKey{}).(*unreadCollector)
	if !ok || c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range items {
		if item.Project != "" {
			c.items[item.Project] = item.Reason
		}
	}
}

func (c *unreadCollector) list() []UnreadProject {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]UnreadProject, 0, len(c.items))
	for project, reason := range c.items {
		out = append(out, UnreadProject{project, reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out
}

// issueRowProject returns the project of an Issue row key ("issue:owner/repo#12").
func issueRowProject(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "issue:")
	if !ok {
		return "", false
	}
	i := strings.LastIndex(rest, "#")
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

func unreadSet(s Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, u := range s.UnreadProjects {
		out[u.Project] = true
	}
	return out
}
