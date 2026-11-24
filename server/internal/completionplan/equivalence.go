package completionplan

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Comparison is a controller-generated audit of a candidate. Equal candidates
// do not replace the presented plan or retarget its receipt.
type Comparison struct {
	Rule                     string         `json:"rule"`
	Presented                string         `json:"presented"`
	PresentedPlan            map[string]any `json:"presentedPlan"`
	Candidate                string         `json:"candidate"`
	CandidatePlan            map[string]any `json:"candidatePlan"`
	Equivalent               bool           `json:"equivalent"`
	PresentedReplyCompatible bool           `json:"presentedReplyCompatible"`
	Changes                  []string       `json:"changes"`
	Explanation              string         `json:"explanation"`
}

// displayText normalizes only the documented terminal presentation changes.
func displayText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "。")
	if strings.HasSuffix(s, "します") {
		s = strings.TrimSuffix(s, "します") + "する"
	}
	return s
}

// Only a standalone, trailing issue-plan number is a known assignment note.
// Other parentheses, including nested or ambiguous notes, remain significant.
var planNote = regexp.MustCompile(`\s*[（(]案[0-9０-９]+[）)]\s*$`)

func approvalTarget(s string) string {
	s = strings.TrimSpace(s)
	note := planNote.FindString(s)
	if note != "" {
		trimmed := strings.TrimSpace(note)
		if (strings.HasPrefix(trimmed, "（") && strings.HasSuffix(trimmed, "）")) || (strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")")) {
			base := strings.TrimSpace(strings.TrimSuffix(s, note))
			if base != "" {
				return base
			}
		}
	}
	return s
}

// Compare records prose changes, but only the approval boundary and valid plan
// structure determine whether a new confirmation is needed.
func Compare(presented, candidate Plan) Comparison {
	c := Comparison{Rule: "completion-plan-instruction-v2", Presented: presented.Fingerprint(), PresentedPlan: presented.Record(), Candidate: candidate.Fingerprint(), CandidatePlan: candidate.Record(), Changes: []string{}, Explanation: "承認事項の対象・操作・条件・分類・時点を比較し、対象末尾の案番号注記のみ除く。basisと工程の差は監査記録に残し、再確認の理由にしない。了承は提示計画に結び付く。"}
	protected := false
	add := func(path string, critical bool) {
		c.Changes = append(c.Changes, path)
		protected = protected || critical
	}
	for _, p := range []Plan{presented, candidate} {
		if _, err := Parse(p.Record()); err != nil {
			add("format", true)
			break
		}
	}
	if presented.Basis != candidate.Basis {
		add("basis", false)
	}
	steps := map[string]Step{}
	for _, s := range candidate.Steps {
		steps[s.Phase] = s
	}
	for _, s := range presented.Steps {
		o := steps[s.Phase]
		for _, f := range [][3]string{{"owner", s.Owner, o.Owner}, {"prerequisite", s.Prerequisite, o.Prerequisite}, {"evidence", s.Evidence, o.Evidence}} {
			if f[1] != f[2] {
				add("steps."+s.Phase+"."+f[0], false)
			}
		}
	}
	approvals := map[string]Approval{}
	for _, a := range candidate.Approvals {
		approvals[a.ID] = a
	}
	for _, a := range presented.Approvals {
		o, ok := approvals[a.ID]
		path := "approvals." + a.ID
		if !ok {
			add(path+".removed", NeedsUser(a.Category))
			continue
		}
		delete(approvals, a.ID)
		// Items that do not wait for the user are audited, not reconfirmed.
		critical := NeedsUser(a.Category) || NeedsUser(o.Category)
		if a.Category != o.Category || a.Phase != o.Phase {
			add(path+".category/phase", critical)
		}
		if (a.Gate == nil) != (o.Gate == nil) || (a.Gate != nil && o.Gate != nil && *a.Gate != *o.Gate) {
			add(path+".gate", true)
		}
		if !slices.Equal(a.Children, o.Children) {
			add(path+".children", critical)
		}
		if approvalTarget(a.Target) != approvalTarget(o.Target) {
			add(path+".target", critical)
		}
		if displayText(a.Operation) != displayText(o.Operation) {
			add(path+".operation", critical)
		}
		if displayText(a.Condition) != displayText(o.Condition) {
			add(path+".condition", critical)
		}
	}
	for _, a := range candidate.Approvals {
		if _, ok := approvals[a.ID]; ok {
			add(fmt.Sprintf("approvals.%s.added", a.ID), NeedsUser(a.Category))
		}
	}
	c.Equivalent = !protected
	return c
}

// A reply is bound to its verified presentation, not the rewritten candidate.
func ComparePresentedReply(presented, candidate Plan) Comparison {
	c := Compare(presented, candidate)
	c.Rule = "completion-plan-presented-reply-v2"
	c.PresentedReplyCompatible = c.Equivalent
	return c
}
