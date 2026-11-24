package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// 全履歴だけを共有し、開閉などで絞った取得は混ぜない。
// 旧shared/src/gitea-issues.tsと同じく5分全件・その間は1秒重なる差分。
type issueHistory struct {
	mu    sync.Mutex
	repos map[string]*issueSnapshot
	now   func() time.Time
}
type issueSnapshot struct {
	rows    map[int64]map[string]any
	markers map[string]map[int64]bool
	since   string
	fullAt  time.Time
	pending *issueRead
}
type issueRead struct {
	done chan struct{}
	err  error
}

func newIssueHistory() *issueHistory {
	return &issueHistory{repos: map[string]*issueSnapshot{}, now: time.Now}
}
func cloneIssueRows(rows []map[string]any) ([]map[string]any, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		value, ok := cloneIssueValue(row, make(map[cloneVisit]bool), 2)
		if !ok {
			return cloneIssueRowsJSON(rows)
		}
		if value != nil {
			out[i] = value.(map[string]any)
		}
	}
	return out, nil
}

type cloneVisit struct {
	kind reflect.Kind
	ptr  uintptr
}

func cloneIssueValue(value any, active map[cloneVisit]bool, depth int) (any, bool) {
	switch v := value.(type) {
	case nil, bool:
		return v, true
	case string:
		return v, utf8.ValidString(v)
	case json.Number:
		s := string(v)
		if s == "" { // encoding/json encodes an empty Number as zero.
			return json.Number("0"), true
		}
		return v, (s[0] == '-' || s[0] >= '0' && s[0] <= '9') && s[len(s)-1] >= '0' && s[len(s)-1] <= '9' && json.Valid([]byte(s))
	case map[string]any:
		if v == nil {
			return nil, true
		}
		if depth > 10000 {
			return nil, false
		}
		visit := cloneVisit{reflect.Map, uintptr(reflect.ValueOf(v).UnsafePointer())}
		if active[visit] {
			return nil, false
		}
		active[visit] = true
		defer delete(active, visit)
		copy := make(map[string]any, len(v))
		for key, item := range v {
			if !utf8.ValidString(key) {
				return nil, false
			}
			cloned, ok := cloneIssueValue(item, active, depth+1)
			if !ok {
				return nil, false
			}
			copy[key] = cloned
		}
		return copy, true
	case []any:
		if v == nil {
			return nil, true
		}
		if depth > 10000 {
			return nil, false
		}
		visit := cloneVisit{reflect.Slice, uintptr(reflect.ValueOf(v).UnsafePointer())}
		if active[visit] {
			return nil, false
		}
		active[visit] = true
		defer delete(active, visit)
		copy := make([]any, len(v))
		for i, item := range v {
			cloned, ok := cloneIssueValue(item, active, depth+1)
			if !ok {
				return nil, false
			}
			copy[i] = cloned
		}
		return copy, true
	default:
		return nil, false
	}
}

func cloneIssueRowsJSON(rows []map[string]any) ([]map[string]any, error) {
	raw, e := json.Marshal(rows)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	e = decoder.Decode(&out)
	return out, e
}

var IssueMarker = regexp.MustCompile(`<!-- issue-desk:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) -->`)

// DraftID returns the last valid intake marker in a body.
func DraftID(body string) string {
	matches := IssueMarker.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// merge validates the whole response before changing the snapshot.
func (snapshot *issueSnapshot) merge(updates []map[string]any, full bool, started time.Time) error {
	numbers := make([]int64, len(updates))
	for i, row := range updates {
		n, err := strconv.ParseInt(fmt.Sprint(row["number"]), 10, 64)
		if err != nil || n <= 0 || n > 9007199254740991 {
			return errors.New("Issue履歴の番号が不正です")
		}
		numbers[i] = n
	}
	if full {
		snapshot.rows = map[int64]map[string]any{}
		snapshot.markers = map[string]map[int64]bool{}
	}
	for i, row := range updates {
		n := numbers[i]
		if old := snapshot.rows[n]; old != nil {
			body, _ := old["body"].(string)
			key := DraftID(body)
			if key != "" {
				delete(snapshot.markers[key], n)
				if len(snapshot.markers[key]) == 0 {
					delete(snapshot.markers, key)
				}
			}
		}
		snapshot.rows[n] = row
		body, _ := row["body"].(string)
		key := DraftID(body)
		if key != "" {
			if snapshot.markers[key] == nil {
				snapshot.markers[key] = map[int64]bool{}
			}
			snapshot.markers[key][n] = true
		}
	}
	snapshot.since = started.Add(-time.Second).UTC().Format(time.RFC3339Nano)
	if full {
		snapshot.fullAt = started
	}
	return nil
}

type selectedIssueRow struct {
	number int64
	row    map[string]any
}

// selectRows captures immutable published rows while the history lock is held.
func (snapshot *issueSnapshot) selectRows(marker *string) []selectedIssueRow {
	rows := make([]selectedIssueRow, 0)
	if marker == nil {
		for n, row := range snapshot.rows {
			rows = append(rows, selectedIssueRow{n, row})
		}
	} else {
		for n := range snapshot.markers[*marker] {
			rows = append(rows, selectedIssueRow{n, snapshot.rows[n]})
		}
	}
	return rows
}

func copySelectedRows(selected []selectedIssueRow, filingSummary bool) ([]map[string]any, error) {
	sort.Slice(selected, func(i, j int) bool { return selected[i].number > selected[j].number })
	rows := make([]map[string]any, len(selected))
	for i, row := range selected {
		fields := row.row
		// Filing checks use a closed issue only to suppress a closed job
		// and to resolve the name and state of a filed sibling.
		if filingSummary && fields["state"] == "closed" && fields["pull_request"] == nil {
			fields = map[string]any{"number": fields["number"], "state": fields["state"], "title": fields["title"]}
		}
		rows[i] = fields
	}
	return cloneIssueRows(rows)
}

func (c *Client) issueHistory(ctx context.Context, repo, base string) ([]map[string]any, error) {
	return c.issueHistorySelected(ctx, repo, base, nil, false)
}

func (c *Client) issueHistorySelected(ctx context.Context, repo, base string, marker *string, filingSummary bool) ([]map[string]any, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	h := c.history
	if h == nil {
		return nil, errors.New("Issue履歴の取得元が未初期化です")
	}
	h.mu.Lock()
	state := h.repos[repo]
	if state == nil {
		state = &issueSnapshot{}
		h.repos[repo] = state
	}
	if pending := state.pending; pending != nil {
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
		}
		if pending.err != nil {
			// The shared read belongs to its initiating caller. A periodic
			// caller may be stopped while an accepted request is waiting for
			// the same history; retry under the still-live caller's context.
			if ctx.Err() == nil && (errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)) {
				return c.issueHistorySelected(ctx, repo, base, marker, filingSummary)
			}
			return nil, pending.err
		}
		h.mu.Lock()
		rows := state.selectRows(marker)
		h.mu.Unlock()
		return copySelectedRows(rows, filingSummary)
	}
	pending := &issueRead{done: make(chan struct{})}
	state.pending = pending
	started := h.now()
	full := state.rows == nil || started.Before(state.fullAt) || started.Sub(state.fullAt) >= 5*time.Minute
	since := state.since
	h.mu.Unlock()
	route := base + "/issues?type=issues&state=all"
	if !full {
		route += "&since=" + url.QueryEscape(since)
	}
	updates, e := c.Pages(ctx, route)
	if e == nil {
		e = ctx.Err()
	}
	h.mu.Lock()
	if e == nil {
		e = state.merge(updates, full, started)
	}
	pending.err = e
	state.pending = nil
	close(pending.done)
	if e != nil {
		h.mu.Unlock()
		return nil, e
	}
	rows := state.selectRows(marker)
	h.mu.Unlock()
	return copySelectedRows(rows, filingSummary)
}
