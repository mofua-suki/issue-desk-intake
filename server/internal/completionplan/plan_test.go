package completionplan

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture() Plan {
	p := Plan{Version: 1, Basis: "owner/appの接続変更。既存の認証条件を維持する", Approvals: []Approval{}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, Step{phase, "担当コントローラー", "固定した受入条件と隔離検証環境", "対象の成功結果と版を照合する"})
	}
	return p
}

func TestPresentationSnapshotAndReceiptSurviveSerialization(t *testing.T) {
	for _, source := range []string{"screen-session", "pc-token", "cli-key"} {
		for _, reply := range []string{"A", "1-A", "承認します", "この計画を承認します"} {
			p := gated()
			rows := messages(p, reply, "user", source)
			rows[0].(map[string]any)["completionPlan"] = p.Record()
			changed := gated()
			changed.Basis = "次のAI候補が変更した前提"
			raw, _ := json.Marshal(rows)
			var saved any
			_ = json.Unmarshal(raw, &saved)
			actual, receipt, ok := PresentedAfter(changed, saved, 0)
			if !ok || actual.Fingerprint() != p.Fingerprint() || receipt.Fingerprint != p.Fingerprint() || receipt.MessageID != "user-1" || receipt.Quote != reply || receipt.At != "2026-11-07T10:00:00+09:00" || receipt.Source != source {
				t.Fatal(actual, receipt, ok)
			}
			if _, err := Check(actual.Record(), saved, receipt); err != nil {
				t.Fatal(err)
			}
			if _, err := Check(changed.Record(), saved, receipt); err == nil {
				t.Fatal("retargeted receipt accepted")
			}
		}
	}
}

func TestMissingOrInconsistentPresentationCannotBeInvented(t *testing.T) {
	p := gated()
	for _, change := range []func([]any){
		func(rows []any) { delete(rows[0].(map[string]any), "completionProposal") },
		func(rows []any) { rows[0].(map[string]any)["completionPlan"] = nil },
		func(rows []any) { rows[0].(map[string]any)["completionProposal"] = "invented" },
		func(rows []any) { rows[1].(map[string]any)["at"] = "invalid" },
		func(rows []any) { rows[1].(map[string]any)["id"] = "" },
	} {
		rows := messages(p, "A", "user", "screen-session")
		change(rows)
		if _, _, ok := PresentedAfter(p, rows, 0); ok {
			t.Fatal("invalid presentation accepted", rows)
		}
	}
	if _, _, ok := PresentedAfter(Plan{}, messages(p, "A", "user", "cli-key"), 0); ok {
		t.Fatal("missing legacy plan reconstructed")
	}
	if _, _, ok := PresentedAfter(p, messages(p, "A", "user", "cli-key"), 2); ok {
		t.Fatal("reply older than reconfirmation boundary reused")
	}
}

func TestEquivalenceAllowsOnlyDocumentedPresentationChanges(t *testing.T) {
	p := gated()
	p.Basis = "既存の認証条件を維持する。根拠は固定ref " + strings.Repeat("a", 40) + "の確認済みコードと試験本文"
	for _, change := range []func(*Plan){
		func(p *Plan) { p.Basis = strings.ReplaceAll(p.Basis, strings.Repeat("a", 40), strings.Repeat("b", 40)) },
		func(p *Plan) { p.Steps[0].Evidence = "対象の成功結果と版を照合します。" },
		func(p *Plan) {
			p.Basis = strings.ReplaceAll(p.Basis, strings.Repeat("a", 40), strings.Repeat("b", 40))
			p.Steps[0].Evidence = "対象の成功結果と版を照合します。"
			p.Steps[0], p.Steps[3] = p.Steps[3], p.Steps[0]
		},
	} {
		candidate, _ := Parse(p.Record())
		change(&candidate)
		comparison := Compare(p, candidate)
		if !comparison.Equivalent || comparison.Rule == "" || comparison.Explanation == "" || comparison.Presented != p.Fingerprint() || comparison.Candidate != candidate.Fingerprint() {
			t.Fatal(comparison)
		}
	}
}

func TestEquivalenceRejectsApprovalRouteAndFactualChanges(t *testing.T) {
	p := gated()
	for _, change := range []func(*Plan){
		func(p *Plan) { p.Approvals[0].Target = "owner/other" },
		func(p *Plan) { p.Approvals[0].Operation = "削除" },
		func(p *Plan) { p.Approvals[0].Condition = "本番環境" },
		func(p *Plan) { p.Approvals[0].Category = "destructive" },
		func(p *Plan) { p.Approvals[0].Phase = "after_diff" },
		func(p *Plan) { p.Approvals = []Approval{} },
		func(p *Plan) { a := p.Approvals[0]; a.ID = "new"; p.Approvals = append(p.Approvals, a) },
	} {
		candidate, _ := Parse(p.Record())
		change(&candidate)
		if c := Compare(p, candidate); c.Equivalent || len(c.Changes) == 0 {
			t.Fatal("meaningful or unknown change accepted", c)
		}
	}
	old := gated()
	old.Approvals[0].Target = "根拠は固定ref " + strings.Repeat("a", 40) + "の確認済みコードと試験本文"
	candidate, _ := Parse(old.Record())
	candidate.Approvals[0].Target = strings.ReplaceAll(old.Approvals[0].Target, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if Compare(old, candidate).Equivalent {
		t.Fatal("protected target SHA change was ignored")
	}
}
func messages(p Plan, reply, role, source string) []any {
	return []any{map[string]any{"role": "assistant", "completionProposal": p.Fingerprint()}, map[string]any{"id": "user-1", "role": role, "text": reply, "at": "2026-11-07T10:00:00+09:00", "source": source}}
}
func gated() Plan {
	p := fixture()
	p.Approvals = []Approval{{Category: "permission", ID: "deploy", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境"}}
	return p
}
func TestPlanRequiresCompleteRouteAndExplicitApprovalClassification(t *testing.T) {
	if _, err := Parse(fixture().Record()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Plan){func(p *Plan) { p.Steps = p.Steps[:3] }, func(p *Plan) { p.Steps[3].Phase = "implementation" }, func(p *Plan) { p.Steps[1].Owner = "" }, func(p *Plan) {
		p.Approvals = []Approval{{ID: "p", Phase: "after_diff", Target: "auth.go", Operation: "権限境界変更", Condition: "head照合"}}
	}} {
		p := fixture()
		change(&p)
		if _, err := Parse(p.Record()); err == nil {
			t.Fatal("incomplete plan accepted", p)
		}
	}
}
func TestExplicitConfirmationIsBoundToPresentedPlanAndAuthenticatedSender(t *testing.T) {
	p := gated()
	for _, source := range []string{"screen-session", "pc-token", "cli-key"} {
		rows := messages(p, "A", "user", source)
		r, ok := Confirmed(p, rows)
		if !ok || r.Source != source {
			t.Fatal(source, r, ok)
		}
		if _, err := Check(p.Record(), rows, r); err != nil {
			t.Fatal(err)
		}
		changed := p
		changed.Basis = "認証条件を変更する"
		if _, err := Check(changed.Record(), rows, r); err == nil {
			t.Fatal("changed basis reused an approval")
		}
	}
	for _, x := range [][3]string{{"B", "user", "screen-session"}, {"承認しません", "user", "screen-session"}, {"A", "proxy", "screen-session"}, {"A", "user", ""}} {
		if _, ok := Confirmed(p, messages(p, x[0], x[1], x[2])); ok {
			t.Fatal("untrusted or negative accepted", x)
		}
	}
	if _, err := Check(p.Record(), messages(p, "A", "user", "screen-session"), Receipt{}); err == nil {
		t.Fatal("invented receipt accepted")
	}
}
func TestEarlierExplicitInstructionDoesNotRequireSecondConfirmation(t *testing.T) {
	p := gated()
	quote := "owner/appの設定変更を隔離環境で進めてください"
	p.Approvals[0].Evidence = &Evidence{MessageID: "original", Quote: quote, At: "2026-11-07T10:00:00+09:00"}
	rows := []any{map[string]any{"id": "original", "role": "user", "text": quote, "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	receipt, ok := Confirmed(p, rows)
	if !ok || receipt.Quote != quote {
		t.Fatal(receipt, ok)
	}
	if _, err := Check(p.Record(), rows, receipt); err != nil {
		t.Fatal(err)
	}
	copy := p
	copy.Approvals = append([]Approval{}, p.Approvals...)
	copy.Approvals[0].Condition = "本番環境"
	if _, ok := Confirmed(copy, rows); ok {
		t.Fatal("a condition absent from original quote was approved")
	}
	rows = append(rows, map[string]any{"role": "user", "text": "先ほどの了承は取り消します"})
	if _, ok := Confirmed(p, rows); ok {
		t.Fatal("revoked instruction accepted")
	}
}
func TestFinalDiffApprovalIsVisibleButNeverPreauthorized(t *testing.T) {
	p := fixture()
	p.Approvals = []Approval{{Category: "destructive", ID: "delete", Phase: "after_diff", Target: "削除SQLの差分", Operation: "削除を承認してマージ", Condition: "対象headとSQLを確認する"}}
	if RequiresConfirmation(p) {
		t.Fatal("final-diff approval incorrectly blocks implementation")
	}
	if _, err := Check(p.Record(), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"差分・対象SHA", "削除SQL", "認可を代替しません"} {
		if !strings.Contains(p.Section(), part) {
			t.Fatal(p.Section())
		}
	}
}

func TestConditionalReplyCannotFallBackToOlderQuotedApproval(t *testing.T) {
	for _, condition := range []string{"ただし本番は触らずに", "但し本番は触らずに"} {
		t.Run(condition, func(t *testing.T) {
			p := gated()
			quote := "owner/appの設定変更を隔離環境で進めてください"
			p.Approvals[0].Evidence = &Evidence{MessageID: "original", Quote: quote, At: "2026-11-07T10:00:00+09:00"}
			rows := []any{map[string]any{"id": "original", "role": "user", "text": quote, "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}, map[string]any{"role": "assistant", "completionProposal": p.Fingerprint()}, map[string]any{"role": "user", "text": condition}}
			if _, ok := Confirmed(p, rows); ok {
				t.Fatal("conditional reply resurrected the original approval")
			}
		})
	}
}

func TestPlanWithoutGateCompatibility(t *testing.T) {
	// Fixed wire contract, including field order and the evidence-free fingerprint.
	const wire = `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	var saved any
	if err := json.Unmarshal([]byte(wire), &saved); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		p, err := Parse(saved)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(p)
		if err != nil || string(raw) != wire || p.Approvals[0].Gate != nil || p.Fingerprint() != "a44a3ebaf86948b4f5da54e6db4c75aabde9e072b16dda0d81a53dbc3acd6dec" {
			t.Fatalf("wire=%s fingerprint=%s err=%v", raw, p.Fingerprint(), err)
		}
		saved = p.Record()
	}
}

func TestPlanExistingGateValidation(t *testing.T) {
	for _, tc := range []struct {
		name, phase, category string
		gate                  any
		unknown               bool
		valid                 bool
	}{
		{"permission", "after_diff", "permission", "permission", false, true},
		{"deletion", "after_diff", "destructive", "deletion", false, true},
		{"permission-scope", "after_diff", "scope", "permission", false, false},
		{"permission-irreversible", "after_diff", "irreversible", "permission", false, false},
		{"deletion-permission", "after_diff", "permission", "deletion", false, false},
		{"deletion-scope", "after_diff", "scope", "deletion", false, false},
		{"unknown", "after_diff", "permission", "other", false, false},
		{"empty", "after_diff", "permission", "", false, false},
		{"whitespace", "after_diff", "permission", " permission ", false, false},
		{"number", "after_diff", "permission", 1, false, false},
		{"before-permission", "before_implementation", "permission", "permission", false, false},
		{"before-deletion", "before_implementation", "destructive", "deletion", false, false},
		{"unknown-field", "after_diff", "permission", "permission", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gated().Record()
			a := r["approvals"].([]any)[0].(map[string]any)
			a["phase"], a["category"], a["gate"] = tc.phase, tc.category, tc.gate
			if tc.unknown {
				a["unexpected"] = true
			}
			p, err := Parse(r)
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "完了計画の形式が不正です") {
					t.Fatalf("invalid gate accepted: %+v %v", p, err)
				}
				return
			}
			if err != nil || p.Approvals[0].Gate == nil || *p.Approvals[0].Gate != tc.gate {
				t.Fatalf("valid gate rejected: %+v %v", p, err)
			}
			// The stored JSON and its reload retain the marker and its fingerprint.
			reloaded, err := Parse(p.Record())
			if err != nil || reloaded.Approvals[0].Gate == nil || *reloaded.Approvals[0].Gate != tc.gate || reloaded.Fingerprint() != p.Fingerprint() {
				t.Fatalf("gate lost on reload: %+v %v", reloaded, err)
			}
		})
	}
}

func TestPlanSectionExistingGates(t *testing.T) {
	p := gated()
	p.Approvals[0].Phase = "after_diff"
	plain := p.Section()
	if !strings.Contains(plain, "具体的な差分・対象SHAが確定した後に最終承認") {
		t.Fatal(plain)
	}
	for _, tc := range []struct{ gate, category, label string }{{"permission", "permission", "権限保護"}, {"deletion", "destructive", "削除承認"}} {
		p.Approvals[0].Category = tc.category
		p.Approvals[0].Gate = &tc.gate
		parsed, err := Parse(p.Record())
		if err != nil {
			t.Fatal(err)
		}
		section := parsed.Section()
		if !strings.Contains(section, "既存の"+tc.label+"ゲートに委ね、計画上の最終承認は別途求めない") || strings.Contains(section, "具体的な差分・対象SHAが確定した後に最終承認") {
			t.Fatal(section)
		}
	}
	p.Approvals[0].Category = "permission"
	p.Approvals[0].Gate = nil
	if p.Section() != plain {
		t.Fatal("gateなしの表示が変化した")
	}
}

func TestPlanApprovalChildren(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children any
		valid    bool
		applies  []int
	}{
		{"omitted", nil, true, []int{-1, 0, 1, 2, 25, 26}},
		{"null", nil, true, []int{-1, 0, 1, 2, 25, 26}},
		{"empty", []int{}, true, []int{-1, 0, 1, 2, 25, 26}},
		{"one", []int{1}, true, []int{-1, 0, 1}},
		{"two", []int{1, 2}, true, []int{-1, 0, 1, 2}},
		{"upper-bound", []int{25}, true, []int{-1, 0, 25}},
		{"zero", []int{0}, false, nil},
		{"negative", []int{-1}, false, nil},
		{"duplicate", []int{1, 1}, false, nil},
		{"out-of-range", []int{26}, false, nil},
		{"fraction", []float64{1.5}, false, nil},
		{"string", []string{"1"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gated().Record()
			if tc.name != "omitted" {
				r["approvals"].([]any)[0].(map[string]any)["children"] = tc.children
			}
			p, err := Parse(r)
			if !tc.valid {
				if err == nil {
					t.Fatalf("invalid children accepted: %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := Parse(p.Record())
			if err != nil || reloaded.Fingerprint() != p.Fingerprint() {
				t.Fatalf("reload: %+v %v", reloaded, err)
			}
			for _, index := range []int{-1, 0, 1, 2, 25, 26} {
				want := false
				for _, applicable := range tc.applies {
					want = want || index == applicable
				}
				if got := reloaded.Approvals[0].AppliesTo(index); got != want {
					t.Fatalf("AppliesTo(%d)=%v want %v", index, got, want)
				}
			}
			if tc.name == "omitted" || tc.name == "null" || tc.name == "empty" {
				if _, exists := p.Record()["approvals"].([]any)[0].(map[string]any)["children"]; exists || p.Fingerprint() != gated().Fingerprint() {
					t.Fatal("unrestricted approval changed its stored contract or fingerprint")
				}
			}
		})
	}
}

func TestPlanApprovalChildrenReconfirmation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, to   []int
		equivalent bool
	}{
		{"restrict", nil, []int{2}, false},
		{"retarget", []int{2}, []int{1}, false},
		{"expand", []int{2}, []int{1, 2}, false},
		{"remove", []int{2}, nil, false},
		{"unchanged", []int{2}, []int{2}, true},
		{"unrestricted", nil, []int{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := gated()
			p.Approvals[0].Children = tc.from
			candidate, err := Parse(p.Record())
			if err != nil {
				t.Fatal(err)
			}
			candidate.Approvals[0].Children = tc.to
			comparison := Compare(p, candidate)
			reply := ComparePresentedReply(p, candidate)
			if comparison.Equivalent != tc.equivalent || reply.PresentedReplyCompatible != tc.equivalent {
				t.Fatalf("changed child target reused approval: %+v %+v", comparison, reply)
			}
			if !tc.equivalent && (len(comparison.Changes) != 1 || comparison.Changes[0] != "approvals.deploy.children" || p.Fingerprint() == candidate.Fingerprint()) {
				t.Fatalf("child target change not audited: %+v", comparison)
			}
		})
	}
}

func TestPlanSectionShowsApprovalChildren(t *testing.T) {
	p := gated()
	p.Approvals[0].Children = []int{2, 5}
	section := p.Section()
	if !strings.Contains(section, "対象の子: 2, 5") {
		t.Fatalf("approval target children are missing: %s", section)
	}
	p.Approvals[0].Children = nil
	if strings.Contains(p.Section(), "対象の子:") {
		t.Fatal("unrestricted approval was shown as child-specific")
	}
}
