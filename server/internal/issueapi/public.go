package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/listscope"
)

// List returns the same lightweight issue projection as the HTTP list.
// It fails when any candidate repository cannot be read.
func (s *Service) List(ctx context.Context) ([]Object, error) { return s.list(ctx) }

// ListPartial returns the rows of the readable candidates and the candidates
// that could not be read. Callers must not treat the rows as complete when
// unread is non-empty.
func (s *Service) ListPartial(ctx context.Context) ([]Object, []UnreadProject, error) {
	return s.listFiltered(ctx, "", "")
}
func (s *Service) ListEntry(ctx context.Context, repo string, n int64) (Object, error) {
	if e := s.o.Project(repo); e != nil {
		return nil, e
	}
	issue, e := s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	author, e := s.requestAuthorChecked(issue)
	if e != nil {
		return nil, e
	}
	if issue["pull_request"] != nil || !author || coordination(repo, issue) {
		return nil, fail(403, "この依頼を表示できません。")
	}
	value, _, e := s.row(ctx, repo, issue)
	return value, e
}
func (s *Service) CloseRequests(ctx context.Context) ([]Object, error) {
	return listscope.Read(ctx, "issue-close-requests", func() ([]Object, error) {
		return s.listCloseRequests(ctx)
	})
}
func (s *Service) listCloseRequests(ctx context.Context) ([]Object, error) {
	records, e := s.closeRecords(ctx, closeOpenFilter())
	if e != nil {
		return nil, e
	}
	out := []Object{}
	for _, record := range records {
		if closeRequestOpen(record) {
			view := closeView(record, true)
			_, active := s.closeInFlight.Load(fleet.Text(record["id"]))
			view["executionActive"] = active
			out = append(out, view)
		}
	}
	return out, nil
}
func (s *Service) CloseRequest(ctx context.Context, id string) (Object, error) {
	record, e := s.read(ctx, "close_requests", id)
	if e != nil || record == nil {
		return record, e
	}
	if !closeRequestOpen(record) {
		return nil, fail(404, "このクローズ依頼は一覧の対象ではありません。")
	}
	view := closeView(record, true)
	_, active := s.closeInFlight.Load(fleet.Text(record["id"]))
	view["executionActive"] = active
	return view, nil
}

func closeRequestOpen(record Object) bool {
	return oneOf(fleet.Text(record["status"]), "reviewing", "confirm", "closing")
}
