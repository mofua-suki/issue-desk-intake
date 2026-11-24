package issueapi

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"reflect"
	"strings"
	"testing"
)

// GET /api/issues?scope=active answers from the reader of the active range and
// never from the full list; other scope values are refused before any read.
func TestIssueListScopeQuery(t *testing.T) {
	active := []Object{{"project": "owner/app", "number": float64(2), "status": "running"}}
	var unread []UnreadProject
	var readErr error
	calls := 0
	s := &Service{o: Options{ActiveList: func(context.Context) ([]Object, []UnreadProject, error) {
		calls++
		return active, unread, readErr
	}}}
	status := func(err error) int {
		var problem *contract.Problem
		if errors.As(err, &problem) {
			return problem.Status
		}
		return 0
	}

	rows, err := s.listScoped(context.Background(), Object{"scope": "active"})
	if err != nil || !reflect.DeepEqual(rows, active) || calls != 1 {
		t.Fatalf("rows=%v error=%v calls=%d", rows, err, calls)
	}
	for _, value := range []string{"", "full", "Active", "active,full"} {
		if _, err = s.listScoped(context.Background(), Object{"scope": value}); status(err) != 400 {
			t.Fatalf("scope=%q error=%v", value, err)
		}
	}
	if calls != 1 {
		t.Fatalf("a refused scope read the list: calls=%d", calls)
	}

	// An unread candidate cannot be reported in a bare array.
	unread = []UnreadProject{{Project: "owner/gone", Reason: "読めません"}}
	if _, err = s.listScoped(context.Background(), Object{"scope": "active"}); status(err) != 503 || !strings.Contains(err.Error(), "owner/gone") {
		t.Fatalf("unread candidate error=%v", err)
	}
	unread, readErr = nil, errors.New("database unavailable")
	if _, err = s.listScoped(context.Background(), Object{"scope": "active"}); !errors.Is(err, readErr) {
		t.Fatalf("read failure error=%v", err)
	}

	// No row is not a missing list.
	active, readErr = nil, nil
	if rows, err = s.listScoped(context.Background(), Object{"scope": "active"}); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty rows=%#v error=%v", rows, err)
	}
}
