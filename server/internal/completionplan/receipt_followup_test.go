package completionplan

import (
	"strings"
	"testing"
)

func TestCompletionReceiptSurvivesUnrelatedFollowupForExplicitAndQuotedApproval(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		p := gated()
		rows := messages(p, "A", "user", "screen-session")
		if quoted {
			quote := p.Approvals[0].Target + "の" + p.Approvals[0].Operation + "を" + p.Approvals[0].Condition + "で進めてください"
			rows = rows[1:]
			rows[0].(map[string]any)["text"] = quote
			p.Approvals[0].Evidence = &Evidence{MessageID: "user-1", Quote: quote, At: "2026-11-07T10:00:00+09:00"}
		}
		receipt, ok := Confirmed(p, rows)
		if !ok {
			t.Fatal("initial receipt", quoted)
		}
		for _, followup := range []string{"資料はdocs/x.mdです", "対象はowner/otherへ", "説明して"} {
			next := append(append([]any{}, rows...), map[string]any{"role": "user", "id": "followup", "text": followup})
			if _, err := Check(p.Record(), next, receipt); err != nil {
				t.Fatal("unrelated followup revoked saved terms", quoted, followup, err)
			}
			if !quoted && AllowsCandidateRewrite(receipt, next) != (followup == "説明して") {
				t.Fatal("new instruction treated as AI rewrite", followup)
			}
		}
		for _, followup := range []string{"取消します", "条件を変更します", "条件を変えます", "条件を追加します", "条件を厳しくします", "条件を外します", "条件が違います", "条件はXにします", "受入条件と条件はXにします", "受入条件を厳しくします", "A、ただし本番には触れない"} {
			next := append(append([]any{}, rows...), map[string]any{"role": "user", "id": "followup", "text": followup})
			if _, ok := Confirmed(p, next); ok {
				t.Fatal("revoked receipt retained", quoted, followup)
			}
			if _, err := Check(p.Record(), next, receipt); err == nil || !strings.Contains(err.Error(), "実装前の承認事項を同じ対象・前提で明示確認していません") {
				t.Fatal("cancelled or conditional receipt accepted", quoted, followup, err)
			}
		}
	}
}

// A symbol answer to an assistant row without a presentation (for example a
// question the consultation AI added after approval) neither approves nor revokes.
func TestCompletionSymbolReplyToUnpresentedRowKeepsPresentedReceipt(t *testing.T) {
	p := gated()
	rows := messages(p, "A", "user", "screen-session")
	receipt, ok := Confirmed(p, rows)
	if !ok {
		t.Fatal("initial receipt")
	}
	unpresented := map[string]any{"role": "assistant", "text": "この計画を了承しますか"}
	for _, symbol := range []string{"A", "1-A", "承認します", "この計画を承認します"} {
		next := append(append([]any{}, rows...), unpresented, map[string]any{"role": "user", "id": "later", "text": symbol, "at": "2026-11-07T10:05:00+09:00", "source": "cli-key"})
		if _, err := Check(p.Record(), next, receipt); err != nil {
			t.Fatal("symbol reply to an unpresented row revoked the receipt", symbol, err)
		}
		if _, err := CheckPresented(p.Record(), next, receipt, 0); err != nil {
			t.Fatal("pending check lost the presented receipt", symbol, err)
		}
		if actual, ok := Confirmed(p, next); !ok || actual.MessageID != "user-1" {
			t.Fatal("later symbol became a new approval", symbol, actual)
		}
		if AllowsCandidateRewrite(receipt, next) {
			t.Fatal("later symbol allowed a candidate prose rewrite", symbol)
		}
		// The reply cannot reach an approval older than the reconfirmation start.
		if _, _, ok := PresentedAfter(p, next, 2); ok {
			t.Fatal("reply before reconfirmation start reused", symbol)
		}
		for _, revoke := range []string{"取消します", "B", "A、ただし本番には触れない"} {
			revoked := append(append([]any{}, next...), map[string]any{"role": "user", "id": "revoke", "text": revoke, "at": "2026-11-07T10:06:00+09:00", "source": "cli-key"})
			if _, err := Check(p.Record(), revoked, receipt); err == nil {
				t.Fatal("revocation after symbol reply accepted", revoke)
			}
			reverse := append(append(append([]any{}, rows...), map[string]any{"role": "user", "id": "revoke", "text": revoke}), unpresented, map[string]any{"role": "user", "id": "later", "text": symbol, "at": "2026-11-07T10:05:00+09:00", "source": "cli-key"})
			if _, err := Check(p.Record(), reverse, receipt); err == nil {
				t.Fatal("symbol reply skipped an earlier revocation", revoke)
			}
		}
	}
	// Without an earlier presented reply, an unpresented row never yields approval.
	for _, history := range [][]any{
		{unpresented, map[string]any{"role": "user", "id": "u", "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "cli-key"}},
		{map[string]any{"role": "assistant", "completionPlan": p.Record()}, map[string]any{"role": "user", "id": "u", "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "cli-key"}},
		append(append([]any{}, rows...), map[string]any{"role": "assistant", "completionPlan": p.Record()}, map[string]any{"role": "user", "id": "u", "text": "A", "at": "2026-11-07T10:05:00+09:00", "source": "cli-key"}),
	} {
		if receipt, ok := Confirmed(p, history); ok {
			t.Fatal("missing or partial presentation became an approval", receipt)
		}
	}
}

func TestCompletionReceiptSurvivesFactualAcceptanceFollowupForExplicitAndQuotedApproval(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		p := gated()
		rows := messages(p, "A", "user", "screen-session")
		if quoted {
			q := p.Approvals[0].Target + "の" + p.Approvals[0].Operation + "を" + p.Approvals[0].Condition + "で進めてください"
			rows = rows[1:]
			rows[0].(map[string]any)["text"] = q
			p.Approvals[0].Evidence = &Evidence{MessageID: "user-1", Quote: q, At: "2026-11-07T10:00:00+09:00"}
		}
		receipt, ok := Confirmed(p, rows)
		if !ok {
			t.Fatal("initial approval", quoted)
		}
		for _, word := range []string{"受入条件", "受入基準"} {
			next := append(append([]any{}, rows...), map[string]any{"role": "user", "id": "followup", "text": "終了済みのIssue検索・一覧の一括読取相談は、" + word + "で詳細専用の読取削減を対象外にしています。"})
			actual, ok := Confirmed(p, next)
			if !ok || !sameReceipt(actual, receipt) {
				t.Fatal(quoted, word, actual, receipt)
			}
			if _, err := Check(p.Record(), next, receipt); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCompletionFactualFollowupRejectsChangedPlanAndInvalidProvenance(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		for _, defect := range []string{"fingerprint", "presentation", "id", "at", "source", "quote"} {
			t.Run(map[bool]string{false: "presented/", true: "quoted/"}[quoted]+defect, func(t *testing.T) {
				p := gated()
				rows := messages(p, "A", "user", "screen-session")
				if quoted {
					q := p.Approvals[0].Target + "の" + p.Approvals[0].Operation + "を" + p.Approvals[0].Condition + "で進めてください"
					rows = rows[1:]
					rows[0].(map[string]any)["text"] = q
					p.Approvals[0].Evidence = &Evidence{MessageID: "user-1", Quote: q, At: "2026-11-07T10:00:00+09:00"}
				}
				receipt, ok := Confirmed(p, rows)
				if !ok {
					t.Fatal("initial")
				}
				switch defect {
				case "fingerprint":
					p.Basis += "変更"
				case "presentation":
					if quoted {
						p.Approvals[0].Evidence.MessageID = "missing"
					} else {
						rows[0].(map[string]any)["completionProposal"] = "invalid"
					}
				case "id":
					rows[len(rows)-1].(map[string]any)["id"] = ""
				case "at":
					rows[len(rows)-1].(map[string]any)["at"] = "invalid"
				case "source":
					rows[len(rows)-1].(map[string]any)["source"] = "invalid"
				case "quote":
					rows[len(rows)-1].(map[string]any)["text"] = "invalid"
				}
				rows = append(rows, map[string]any{"role": "user", "text": "受入条件で対象外にしています。"})
				expected := "実装前の承認事項を同じ対象・前提で明示確認していません"
				if quoted && defect == "fingerprint" {
					expected = "保存した計画の了承記録が発言と一致しません"
				}
				if _, err := Check(p.Record(), rows, receipt); err == nil || !strings.Contains(err.Error(), expected) {
					t.Fatal("invalid provenance accepted or wrong error", err)
				}
			})
		}
	}
}

func TestCompletionFactualFollowupRejectsReceiptBeforeReconfirmation(t *testing.T) {
	p := gated()
	rows := messages(p, "A", "user", "screen-session")
	receipt, ok := Confirmed(p, rows)
	if !ok {
		t.Fatal("initial")
	}
	after := len(rows)
	rows = append(rows, map[string]any{"role": "user", "text": "受入条件で対象外にしています。"})
	_, err := CheckPresented(p.Record(), rows, receipt, after)
	if err == nil || !strings.Contains(err.Error(), "変更した前提への明示回答と保存した了承記録を確認していません") {
		t.Fatal(err)
	}
}

func TestCompletionReceiptSurvivesUnchangedPlanFollowup(t *testing.T) {
	p := gated()
	rows := messages(p, "A", "user", "screen-session")
	receipt, ok := Confirmed(p, rows)
	if !ok {
		t.Fatal("initial receipt")
	}
	for followup, keep := range map[string]bool{
		"完了計画は変更しません。起票を確定してください":   true,
		"計画を変更せずに確定してください":          true,
		"計画は変えずに確定してください":           true,
		"計画は変更しませんが、検証は省いてください":     false,
		"計画は変更せずに進めて。ただし配置は外してください": false,
		"配置の担当を変更します":               false,
	} {
		next := append(append([]any{}, rows...), map[string]any{"role": "user", "id": "followup", "text": followup})
		if _, err := Check(p.Record(), next, receipt); (err == nil) != keep {
			t.Fatal("unchanged followup", followup, keep, err)
		}
	}
}
