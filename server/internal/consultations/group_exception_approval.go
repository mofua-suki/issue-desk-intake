package consultations

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
)

// groupExceptionReceipt binds a reply to the pending proposal and records a
// delegated user's statement before it can be interpreted as consent.
func groupExceptionReceipt(d, data Record, p auth.Principal, now time.Time) (Record, error) {
	body := strings.TrimSpace(text(data["text"]))
	if body != "了承します" && body != "承認します" {
		return nil, nil
	}
	proposal := record(d["crossProposal"])
	fingerprint := text(proposal["fingerprint"])
	if fingerprint == "" {
		return nil, nil
	}
	exception := proposal["repositoryGroup"] == true
	for _, raw := range fleet.Items(proposal["scopes"]) {
		if strings.HasPrefix(text(record(raw)["scope"]), "除外理由: ") || strings.HasPrefix(text(record(raw)["scope"]), "除外:") || strings.HasPrefix(text(record(raw)["scope"]), "除外：") {
			exception = true
		}
	}
	if !exception {
		return nil, nil
	}
	messages := fleet.Items(d["messages"])
	if len(messages) == 0 || record(messages[len(messages)-1])["role"] != "assistant" || text(record(messages[len(messages)-1])["crossProposal"]) != fingerprint {
		return nil, nil
	}
	if p.Kind == auth.Screen {
		expected, ok := chatRevision(data["expectedRevision"])
		current, currentOK := integer(d["revision"])
		if !ok || !currentOK || expected != current {
			return nil, refuse(409, "了承対象の会話を読み直してください。")
		}
	}
	if p.Kind == auth.PC && text(data["approvalFingerprint"]) != fingerprint {
		return nil, refuse(409, "了承対象が変わったため再確認してください。")
	}
	approval, denied := auth.UserApproval(p, data["statement"], data["instructionAt"], now, "組の変更への了承")
	if denied != nil {
		return nil, denied
	}
	receipt := Record{"fingerprint": fingerprint, "via": "screen"}
	if p.Kind == auth.PC {
		receipt["via"], receipt["pc"], receipt["statement"], receipt["instructionAt"] = "chat", p.Name, approval.Statement, approval.InstructionAt
	}
	return receipt, nil
}
