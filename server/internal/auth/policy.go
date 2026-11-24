package auth

import (
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
)

// Policy is chosen by each route from its API contract. The zero policy denies
// all credentials. Delegation is never a substitute for a screen-only approval.
type Policy struct {
	PC                bool
	Hostd             bool
	Screen            bool
	CLI               bool
	RequireDelegation bool
	Host              string
}

// Authorize validates credential class and sender identity only. It cannot
// replace resource ownership, permitted-project, consent, or state validation.
func Authorize(principal Principal, policy Policy) *contract.Problem {
	allowed := principal.Kind == PC && policy.PC || principal.Kind == Hostd && policy.Hostd || principal.Kind == Screen && policy.Screen || principal.Kind == CLI && policy.CLI
	if !allowed {
		return rejected(403, "この認証方法では操作できません。")
	}
	if policy.RequireDelegation && principal.Kind == PC && !principal.Delegated {
		return rejected(403, "このトークンでは運用の代行が許されていません。")
	}
	if policy.Host != "" && (principal.Kind != PC && principal.Kind != Hostd || principal.Name != policy.Host) {
		return rejected(403, "送り手と対象PCが一致しません。")
	}
	return nil
}

// ChatApproval is the evidence of a user approval relayed from the chat: the
// user's statement as said (or the shown question and the chosen symbol) and the
// time the user said it. Both are empty for the screen.
type ChatApproval struct {
	Statement     string
	InstructionAt string
}

// UserApproval accepts an approval that only the user may give. The screen
// session is the user. Elsewhere only a PC token with 運用の代行 may relay it,
// and only with the user's own chat statement (contract.MeaningfulApproval) and
// the time the user said it (contract.InstructionAt), which the caller records
// with the approval and the sending PC. The evidence for the screen is always
// empty. The AI's judgement or a timeout never replaces the statement.
func UserApproval(principal Principal, statement, instructionAt any, now time.Time, what string) (ChatApproval, *contract.Problem) {
	if principal.Kind == Screen {
		return ChatApproval{}, nil
	}
	if principal.Kind != PC || !principal.Delegated {
		return ChatApproval{}, rejected(403, what+"は受付画面か、運用の代行を許したトークンから利用者の承認の発言を添えて行ってください。")
	}
	text, ok := statement.(string)
	text = strings.TrimSpace(text)
	if !ok || !contract.MeaningfulApproval(text) || contract.UTF16Length(text) > 2000 {
		return ChatApproval{}, rejected(400, what+"には、利用者がチャットで対象を特定して承認した発言（statement、8〜2000文字、定型語だけは不可）が必要です。")
	}
	at, ok := contract.InstructionAt(instructionAt, now)
	if !ok {
		return ChatApproval{}, rejected(400, what+"には、"+contract.InstructionAtProblem)
	}
	return ChatApproval{Statement: text, InstructionAt: at}, nil
}
