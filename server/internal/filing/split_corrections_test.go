package filing

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/splitplan"
)

func correctionProposals() []any {
	return []any{
		Record{"title": "契約", "purpose": "API契約を定義", "acceptance": []any{"契約を公開する"}, "workType": "feature", "changeScope": Record{"known": true, "paths": []any{"api/service.schema.json"}}, "dependsOn": []any{}},
		Record{"title": "実装A", "purpose": "APIの実装A", "acceptance": []any{"Aが動く"}, "workType": "feature", "changeScope": Record{"known": true, "paths": []any{"server/a.go"}}, "dependsOn": []any{1}},
		Record{"title": "実装B", "purpose": "APIの実装B", "acceptance": []any{"Bが動く"}, "workType": "feature", "changeScope": Record{"known": true, "paths": []any{"server/b.go"}}, "dependsOn": []any{1, 2}},
	}
}

func correctionFixture(t *testing.T, proposals []any) (*Service, *memoryAPI) {
	t.Helper()
	s, api := fixture(t)
	changeCorrectionRecord(t, s, draftID, func(d Record) { d["splitIssues"] = proposals })
	s.options.SplitPlan = PlanSplit
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
		c := configFixture()
		c.Project = func(string) error { return nil }
		c.Records = s.options.Records
		c.ResolvePreceding = func(_ context.Context, child Record) ([]Record, error) {
			out := []Record{}
			for _, raw := range fleet.Items(child["dependsOnIssues"]) {
				out = append(out, Record{"repo": child["project"], "gitea": row(raw)["number"]})
			}
			return out, nil
		}
		return Prepare(ctx, c, d)
	}
	s.options.Check = func(ctx context.Context, child Record) error {
		parent, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(child["parentSplit"]))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("parent missing")
		}
		converted := []splitplan.Proposal{}
		for _, raw := range fleet.Items(parent["splitIssues"]) {
			p := row(raw)
			scope := row(p["changeScope"])
			accepts := []string{}
			for _, a := range fleet.Items(p["acceptance"]) {
				accepts = append(accepts, fleet.Text(a))
			}
			converted = append(converted, splitplan.Proposal{Purpose: fleet.Text(p["purpose"]), Acceptance: accepts, WorkType: fleet.Text(p["workType"]), ChangeScope: splitplan.Scope{Known: scope["known"] == true, Paths: fleet.Items(scope["paths"])}, DependsOn: fleet.Items(p["dependsOn"])})
		}
		_, corrections := splitplan.CorrectDependencies(converted, nil)
		index, _ := integer(child["splitIndex"])
		for _, c := range corrections {
			if int64(c.ProposalNumber) == index {
				return fail(409, "保存された親に不要な依存が残っています")
			}
		}
		return ValidateSplit(ctx, BuildConfig{Project: func(string) error { return nil }, ResolvePreceding: func(context.Context, Record) ([]Record, error) { return []Record{}, nil }}, parent)
	}
	return s, api
}

func changeCorrectionRecord(t *testing.T, s *Service, id string, apply func(Record)) Record {
	t.Helper()
	d, err := s.options.Records.Change(context.Background(), "drafts", id, func(_ *sql.Tx, d Record) (Record, error) { apply(d); return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func getCorrectionRecord(t *testing.T, s *Service, id string) Record {
	t.Helper()
	d, found, err := s.options.Records.Get(context.Background(), "drafts", id)
	if err != nil || !found {
		t.Fatalf("read %s: found=%v err=%v", id, found, err)
	}
	return d
}

// Seed persisted, uncorrected children as left by an earlier interrupted filing.
func seedCorrectionChildren(t *testing.T, s *Service) []string {
	t.Helper()
	parent := getCorrectionRecord(t, s, draftID)
	plan, err := PlanSplit(parent)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	values := []any{}
	for i, child := range plan {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		child["id"], child["parentSplit"], child["splitIndex"] = id, draftID, i+1
		child["status"], child["revision"], child["attempted"] = "submit_error", 0, false
		child["createdAt"], child["updatedAt"] = "2026-11-01T00:00:00.000Z", "2026-11-01T00:00:00.000Z"
		child["_splitBaseDependencies"] = child["dependencies"]
		child["_splitFingerprint"] = splitFingerprint(child)
		_, err = s.options.Records.Change(context.Background(), "drafts", id, func(*sql.Tx, Record) (Record, error) { return child, nil })
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		values = append(values, id)
	}
	changeCorrectionRecord(t, s, draftID, func(d Record) { d["splitChildren"], d["status"] = values, "submit_error" })
	return ids
}

func assertCorrectionHistory(t *testing.T, parent Record, proposal, removed int, before, after string) {
	t.Helper()
	history := fleet.Items(parent["splitDependencyCorrections"])
	if len(history) != 1 {
		t.Fatalf("correction count: %v", history)
	}
	c := row(history[0])
	n, _ := integer(c["proposalNumber"])
	dep, _ := integer(c["removedDependency"])
	if n != int64(proposal) || dep != int64(removed) || fleet.Text(c["reason"]) == "" || fleet.Text(c["at"]) == "" || encoded(c["before"]) != before || encoded(c["after"]) != after {
		t.Fatalf("correction: %v", c)
	}
}

func TestSplitCorrectionNotesStayInConfirmedFactsAcrossProjects(t *testing.T) {
	proposals := correctionProposals()
	row(proposals[2])["project"] = "owner/other"
	parent, err := correctSplitDependencies(Record{"id": draftID, "project": "owner/app", "splitIssues": proposals, "crossProposal": Record{"reason": "契約と二つの実装"}}, nil, "2026-11-09T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanSplit(parent)
	if err != nil {
		t.Fatal(err)
	}
	body := fleet.Text(plan[2]["body"])
	confirmed := strings.Index(body, "## 確認済み事項")
	note := strings.Index(body, "依存の自動補正: 分割案2への依存を外しました")
	cross := strings.Index(body, "## 複数プロジェクトにまたがる理由")
	if confirmed < 0 || note <= confirmed || cross <= note {
		t.Fatal("補正理由が確認済み事項の節にない", body)
	}
}

func TestSplitPlanShowsOnlyCurrentCorrectionOnce(t *testing.T) {
	parent, err := correctSplitDependencies(Record{"project": "owner/app", "splitIssues": correctionProposals()}, nil, "2026-11-09T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	history := fleet.Items(parent["splitDependencyCorrections"])
	parent["splitDependencyCorrections"] = append(history, history[0])
	plan, err := PlanSplit(parent)
	if err != nil {
		t.Fatal(err)
	}
	if body := fleet.Text(plan[2]["body"]); strings.Count(body, "依存の自動補正: 分割案2") != 1 {
		t.Fatal(body)
	}
	row(fleet.Items(parent["splitIssues"])[2])["dependsOn"] = []any{1, 2}
	plan, err = PlanSplit(parent)
	if err != nil {
		t.Fatal(err)
	}
	if body := fleet.Text(plan[2]["body"]); strings.Contains(body, "依存の自動補正: 分割案2") {
		t.Fatal(body)
	}
}
