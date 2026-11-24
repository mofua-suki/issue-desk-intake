package filing

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

func archivedRepositoryMessage(repo string) string {
	return fmt.Sprintf("起票先 %s は保管済みで新しいIssueを受け付けません。同じ内容での再起票では解消しません。登録済みの別プロジェクトを選択するか、この相談を終了してください。", repo)
}

// Stops belong to destinations, independently of the current presentation error.
// Retain earlier destinations when a consultation chooses another project.
func repositoryStop(d Record) error {
	for _, raw := range fleet.Items(d["repositoryStops"]) {
		stop := row(raw)
		if strings.EqualFold(fleet.Text(stop["project"]), fleet.Text(d["project"])) && stop["reason"] == "archived" {
			return contract.NewProblem(409, archivedRepositoryMessage(fleet.Text(stop["project"])), "repository_archived")
		}
	}
	return nil
}

// Called only for a definitive 423 from CreateIssue, never a history/attachment
// failure or an uncertain mutation. Save the evidence with the submission failure.
func (s *Service) repositoryRejection(ctx context.Context, d Record) error {
	repo := fleet.Text(d["project"])
	if s.options.RepositoryArchived != nil {
		archived, err := s.options.RepositoryArchived(ctx, repo)
		if err == nil {
			if !archived {
				return contract.NewProblem(502, fmt.Sprintf("起票先 %s がHTTP 423で作成を拒否しました。保管済みではありません。一時的なロックの可能性があります。再試行できます。", repo), "repository_locked")
			}
			stops := append([]any{}, fleet.Items(d["repositoryStops"])...)
			stops = append(stops, Record{"project": repo, "reason": "archived", "message": archivedRepositoryMessage(repo), "stoppedAt": iso(s.options.Now())})
			d["repositoryStops"] = stops
			return repositoryStop(d)
		}
	}
	return contract.NewProblem(502, fmt.Sprintf("起票先 %s がHTTP 423で作成を拒否しました。保管状態を確認できないため、保管済みによる再送停止は適用していません。状況を確認して再試行してください。", repo), "repository_archive_unverified")
}
