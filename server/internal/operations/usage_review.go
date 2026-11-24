package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

func (s *Service) UsageReviewAlerts(ctx context.Context) ([]any, error) {
	if s.Statistics == nil {
		return nil, contract.NewProblem(503, "利用量の見直しを読めません。", "")
	}
	report, e := s.Statistics.UsageReviewUnacknowledged(ctx)
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, raw := range fleet.List(fleet.Object(report)["rounds"]) {
		r := fleet.Object(raw)
		v := fleet.Object(r["verdict"])
		if v["result"] != "not_recouped" || r["acknowledgedAt"] != nil {
			continue
		}
		out = append(out, map[string]any{"round": r["round"], "title": fmt.Sprintf("利用量の見直し 第%v回: 見直しの費用が減った量を上回っています", r["round"]), "reason": v["reason"], "startedAt": r["startedAt"], "startDate": r["startDate"], "investigationIssue": r["investigationIssue"]})
	}
	return out, nil
}
