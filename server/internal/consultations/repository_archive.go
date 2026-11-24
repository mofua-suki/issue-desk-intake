package consultations

import (
	"context"
	"fmt"

	"issue-desk-intake/server/internal/contract"
)

// CheckRepositoryArchive reports whether the lookup succeeded separately from
// the rejection, so callers can distinguish an unknown state from an active repository.
func (s *Service) CheckRepositoryArchive(ctx context.Context, repo string) (bool, error) {
	if s.options.RepositoryArchived == nil {
		return false, nil
	}
	archived, err := s.options.RepositoryArchived(ctx, repo)
	if err != nil {
		return false, nil
	}
	if archived {
		return true, contract.NewProblem(409, fmt.Sprintf("起票先 %s は保管済みで新しいIssueを受け付けません。同じ内容での再起票では解消しません。登録済みの別プロジェクトを選択するか、この相談を終了してください。", repo), "repository_archived")
	}
	return true, nil
}
