package completionplan

import (
	"reflect"
	"testing"
)

func TestPresentedReplyComparisonAuditsAllFreeTextWithoutClaimingEquivalence(t *testing.T) {
	original := gated()
	candidate, _ := Parse(original.Record())
	candidate.Basis = "候補が再生成した判断根拠"
	want := []string{"basis"}
	for i := range candidate.Steps {
		candidate.Steps[i].Prerequisite = "候補が再生成した前提"
		candidate.Steps[i].Evidence = "候補が再生成した確認方法"
		want = append(want, "steps."+candidate.Steps[i].Phase+".prerequisite", "steps."+candidate.Steps[i].Phase+".evidence")
	}
	strict := Compare(original, candidate)
	actual := ComparePresentedReply(original, candidate)
	if !strict.Equivalent || strict.PresentedReplyCompatible || !actual.Equivalent || !actual.PresentedReplyCompatible || actual.Rule != "completion-plan-presented-reply-v2" || actual.Explanation == "" || !reflect.DeepEqual(actual.Changes, want) || !reflect.DeepEqual(actual.PresentedPlan, original.Record()) || !reflect.DeepEqual(actual.CandidatePlan, candidate.Record()) || actual.Presented != original.Fingerprint() || actual.Candidate != candidate.Fingerprint() {
		t.Fatal(strict, actual)
	}
	// The literal audit must also retain differences normalized by strict Compare.
	candidate, _ = Parse(original.Record())
	candidate.Basis += "。"
	actual = ComparePresentedReply(original, candidate)
	if !Compare(original, candidate).Equivalent || !actual.Equivalent || !actual.PresentedReplyCompatible || !reflect.DeepEqual(actual.Changes, []string{"basis"}) {
		t.Fatal(actual)
	}
}

func TestPresentedReplyComparisonRejectsEveryProtectedFieldAndInvalidFormat(t *testing.T) {
	for name, change := range map[string]func(*Plan){
		"target":          func(p *Plan) { p.Approvals[0].Target += "。" },
		"operation":       func(p *Plan) { p.Approvals[0].Operation = "削除" },
		"condition":       func(p *Plan) { p.Approvals[0].Condition = "本番" },
		"category":        func(p *Plan) { p.Approvals[0].Category = "destructive" },
		"timing":          func(p *Plan) { p.Approvals[0].Phase = "after_diff" },
		"id":              func(p *Plan) { p.Approvals[0].ID = "renamed" },
		"removed":         func(p *Plan) { p.Approvals = []Approval{} },
		"added":           func(p *Plan) { a := p.Approvals[0]; a.ID = "new"; p.Approvals = append(p.Approvals, a) },
		"version":         func(p *Plan) { p.Version = 2 },
		"missing-step":    func(p *Plan) { p.Steps = p.Steps[:3] },
		"duplicate-phase": func(p *Plan) { p.Steps[1].Phase = p.Steps[0].Phase },
		"empty-prose":     func(p *Plan) { p.Basis = "" },
	} {
		t.Run(name, func(t *testing.T) {
			original := gated()
			candidate, _ := Parse(original.Record())
			candidate.Basis = "候補の自由文"
			change(&candidate)
			if c := ComparePresentedReply(original, candidate); c.PresentedReplyCompatible || c.Equivalent || len(c.Changes) == 0 {
				t.Fatal(c)
			}
		})
	}
}

func TestCompareReconfirmOnlyOnInstructionDeviation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		empty     bool
		change    func(*Plan)
		reconfirm bool
	}{
		{"basis", false, func(p *Plan) { p.Basis = "異なる判断の前提" }, false},
		{"steps", false, func(p *Plan) {
			p.Steps[0].Owner = "別担当"
			p.Steps[0].Prerequisite = "前提を整理"
			p.Steps[0].Evidence = "確認を整理"
		}, false},
		{"no approvals", true, func(p *Plan) { p.Basis = "新しい前提"; p.Steps[0].Owner = "別担当" }, false},
		{"split annotation", false, func(p *Plan) { p.Approvals[0].Target += "（案2）" }, false},
		{"changed annotation", false, func(p *Plan) { p.Approvals[0].Target += "（本番環境）" }, true},
		{"unknown annotation", false, func(p *Plan) { p.Approvals[0].Target += "（案2と本番）" }, true},
		{"target added", false, func(p *Plan) { p.Approvals[0].Target += " と owner/other" }, true},
		{"target removed", false, func(p *Plan) { p.Approvals[0].Target = "owner" }, true},
		{"condition expanded", false, func(p *Plan) { p.Approvals[0].Condition = "隔離環境と本番環境" }, true},
		{"operation changed", false, func(p *Plan) { p.Approvals[0].Operation = "削除" }, true},
		{"approval removed", false, func(p *Plan) { p.Approvals = []Approval{} }, true},
		{"approval added", false, func(p *Plan) { a := p.Approvals[0]; a.ID = "other"; p.Approvals = append(p.Approvals, a) }, true},
		{"invalid", false, func(p *Plan) { p.Steps = p.Steps[:3] }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := gated()
			if tc.empty {
				old.Approvals = []Approval{}
			}
			candidate, _ := Parse(old.Record())
			tc.change(&candidate)
			for _, result := range []Comparison{Compare(old, candidate), ComparePresentedReply(old, candidate)} {
				if result.Equivalent == tc.reconfirm {
					t.Fatalf("unexpected comparison: %+v", result)
				}
			}
		})
	}
}

func TestExistingGateComparison(t *testing.T) {
	for _, gate := range []string{"permission", "deletion"} {
		for _, from := range []string{"", gate} {
			for _, to := range []string{"", gate} {
				t.Run(from+"-to-"+to, func(t *testing.T) {
					original := gated()
					if gate == "deletion" {
						original.Approvals[0].Category = "destructive"
					}
					original.Approvals[0].Phase = "after_diff"
					if from != "" {
						value := from
						original.Approvals[0].Gate = &value
					}
					candidate, err := Parse(original.Record())
					if err != nil {
						t.Fatal(err)
					}
					candidate.Approvals[0].Gate = nil
					if to != "" {
						value := to
						candidate.Approvals[0].Gate = &value
					}
					if (original.Fingerprint() == candidate.Fingerprint()) != (from == to) {
						t.Fatal("gate fingerprint does not distinguish values and omission")
					}
					// Existing presentation normalization and prose-only audit still apply.
					candidate.Basis += "。"
					candidate.Approvals[0].Target += "（案2）"
					candidate.Approvals[0].Operation += "。"
					wantChanges := []string{"basis"}
					if from != to {
						wantChanges = append(wantChanges, "approvals.deploy.gate")
					}
					for _, compare := range []func(Plan, Plan) Comparison{Compare, ComparePresentedReply} {
						c := compare(original, candidate)
						if c.Equivalent != (from == to) || !reflect.DeepEqual(c.Changes, wantChanges) || c.PresentedReplyCompatible != (c.Rule == "completion-plan-presented-reply-v2" && from == to) {
							t.Fatalf("gate comparison: %+v", c)
						}
						if from == to {
							changed, err := Parse(candidate.Record())
							if err != nil {
								t.Fatal(err)
							}
							changed.Approvals[0].Condition = "本番環境"
							c = compare(original, changed)
							if c.Equivalent || c.PresentedReplyCompatible || !reflect.DeepEqual(c.Changes, []string{"basis", "approvals.deploy.condition"}) {
								t.Fatalf("same gate concealed protected change: %+v", c)
							}
						}
					}
				})
			}
		}
	}
}
