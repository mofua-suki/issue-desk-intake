package filingcheck

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/splitplan"
	"strings"
)

func toSplitProposal(p Record) splitplan.Proposal {
	scope := obj(p["changeScope"])
	acceptance := []string{}
	for _, item := range fleet.Elements(p["acceptance"]) {
		acceptance = append(acceptance, fleet.Text(item))
	}
	return splitplan.Proposal{Purpose: fleet.Text(p["purpose"]), Acceptance: acceptance, WorkType: fleet.Text(p["workType"]), ChangeScope: splitplan.Scope{Known: scope["known"] == true, Paths: fleet.Elements(scope["paths"])}, DependsOn: fleet.Elements(p["dependsOn"])}
}

func (s *Service) checkSplitDependencies(ctx context.Context, d Record) error {
	parentID := fleet.Text(d["parentSplit"])
	if parentID == "" {
		return nil
	}
	p, found, err := s.options.Records.Get(ctx, "drafts", parentID)
	if err != nil {
		return fmt.Errorf("分割元の相談を読めませんでした: %w", err)
	}
	if !found {
		return nil
	}
	children, proposals := fleet.Elements(p["splitChildren"]), fleet.Elements(p["splitIssues"])
	index := -1
	for i, child := range children {
		if fleet.Text(child) == fleet.Text(d["id"]) && fleet.Text(d["id"]) != "" {
			if index >= 0 {
				return nil
			}
			index = i
		}
	}
	if index < 0 || index >= len(proposals) {
		return nil
	}
	converted := make([]splitplan.Proposal, len(proposals))
	for i, p := range proposals {
		converted[i] = toSplitProposal(obj(p))
	}
	deps, corrections := splitplan.CorrectDependencies(converted, nil)
	removed := []string{}
	for _, correction := range corrections {
		if correction.ProposalNumber == index+1 {
			removed = append(removed, fmt.Sprintf("分割案%d", correction.RemovedDependency))
		}
	}
	if len(removed) == 0 {
		return nil
	}
	kept := []string{}
	for _, dep := range deps[index] {
		kept = append(kept, splitplan.FormatDependency(dep))
	}
	remaining := "なし"
	if len(kept) > 0 {
		remaining = strings.Join(kept, "、")
	}
	return fail(fmt.Sprintf("分割案%dの不要な依存: %s。受付の自動補正が適用されていない状態です。修正後の依存: %s。", index+1, strings.Join(removed, "、"), remaining))
}
