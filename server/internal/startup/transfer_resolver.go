package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
)

const transferHoldNotice = "相談だけのモード（hold）のため、まだ起票していません。この内容で起票するときは POST /api/drafts/{下書きID}/submit（revision を添える）で確定してください。続けて相談する場合は、そのまま返信してください。"

var transferJSONFence = regexp.MustCompile("(?s)^\\s*```json\\s*\\n(.*?)\\n```\\s*$")

func transferAnswer(d consultations.Record) (string, string) {
	switch d["status"] {
	case "intake_waiting", "draft", "submitted":
	default:
		return "", ""
	}
	messages := fleet.List(d["messages"])
	text := ""
	found := false
	for i := len(messages) - 1; i >= 0; i-- {
		m := fleet.Object(messages[i])
		if m["role"] == "assistant" {
			text, found = m["text"].(string)
			break
		}
	}
	if !found {
		return "", ""
	}
	text = strings.TrimSuffix(text, "\n\n"+transferHoldNotice)
	if match := transferJSONFence.FindStringSubmatch(text); match != nil {
		text = match[1]
	}
	var answer map[string]any
	if json.Unmarshal([]byte(text), &answer) != nil {
		return "", ""
	}
	relation, evidence := fleet.Text(answer["relation"]), fleet.Text(answer["evidence"])
	if relation != "required" && relation != "unrelated" || strings.TrimFunc(evidence, func(r rune) bool { return r == '\ufeff' || unicode.IsSpace(r) && r != '\u0085' }) == "" || contract.UTF16Length(evidence) > 4000 {
		return "", ""
	}
	return relation, evidence
}
func transferConsultationText(check jobs.TransferCheck) string {
	raw, _ := json.Marshal(check.Context)
	text := string(raw)
	prefix := "既存Issueの開始条件の照合です。起票・変更・依存解除をせず相談だけで判断してください。依頼の受入条件が移管成果物を利用するか資料を確認してください。producesは自分が作る案なので除き、他の案の成果物の利用を照合してください。関係不明を無関係と推測しないでください。確定できた場合だけreply全体をJSON {\"relation\":\"required\"または\"unrelated\",\"evidence\":\"依頼と移管項目を確認した根拠\"} にしてください。不明なら通常の質問で相談を続けてください。" + fmt.Sprintf("対象 %s#%d、移管元 %s。", strings.ToLower(check.Repo), check.Issue, fleet.Text(check.Context["source"]))
	units := utf16.Encode([]rune(text))
	if len(units) <= 6500 {
		return prefix + "\n照合資料（input.completeも確認）: " + text
	}
	units = units[:6500]
	if units[len(units)-1] >= 0xd800 && units[len(units)-1] <= 0xdbff {
		units = units[:len(units)-1]
	}
	return prefix + "\n以下は先頭6500 UTF-16コード単位以内の抜粋で全文ではありません。対象Issue・移管元の報告の全文は追加取得できません。提供された資料だけで根拠が足りなければ関係を確定せず質問してください。\n" + string(utf16.Decode(units))
}
func (a *App) resolveAcceptanceTransfers(ctx context.Context) error {
	// この周期だけが移管記録の照合と開始条件の更新を定期に行う。照合に失敗しても
	// 開始条件は更新し、相談の照合は次の周期へ回す。
	reconcileErr := a.Jobs.ReconcileTransferRecords(ctx)
	if e := a.Jobs.RefreshTransfers(ctx); e != nil || reconcileErr != nil {
		return errors.Join(reconcileErr, e)
	}
	checks, e := a.Jobs.TransferChecks(ctx)
	if e != nil {
		return e
	}
	if len(checks) == 0 {
		// この周期は照合結果を保存しないので、開始条件をもう一度更新しない。
		// Issue本文を読めなかった依頼は次の周期と配布の直前に読み直す。
		return nil
	}
	failures := []error{}
	for _, check := range checks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = a.resolveTransferCheck(ctx, check); e != nil {
			failures = append(failures, fmt.Errorf("移管成果物の相談を照合できません（job %d）", check.JobID))
		}
	}
	if e = a.Jobs.RefreshTransfers(ctx); e != nil {
		failures = append(failures, e)
	}
	return errors.Join(failures...)
}
func (a *App) resolveTransferCheck(ctx context.Context, check jobs.TransferCheck) error {
	id := jobs.TransferConsultationID(check.JobID, check.Key)
	p := auth.Principal{Kind: auth.Screen}
	draft, found, e := a.Records.Get(ctx, "drafts", id)
	if e != nil {
		return e
	}
	if !found {
		provider := check.Provider
		if provider != "claude" || a.CurrentConfig().Claude == "" {
			provider = "codex"
		}
		// The intake is the requester of this check, so it chooses the tier itself:
		// standard, like its other internal consultations (consultationai/answer.go).
		draft, e = a.Consultations.CreateDraft(ctx, consultations.Record{"id": id, "project": check.Repo, "provider": provider, "hold": true, "tier": "standard"}, p, false)
		if e != nil {
			return e
		}
	}
	if len(fleet.List(draft["messages"])) == 0 {
		if _, e = a.Consultations.Chat(ctx, id, consultations.Record{"messageId": id, "text": transferConsultationText(check)}, p); e != nil {
			return e
		}
	}
	draft, _, e = a.Records.Get(ctx, "drafts", id)
	if e != nil {
		return e
	}
	if draft["status"] == "thinking" && a.Engine != nil {
		if _, e = a.Engine.Start(ctx, id, draft, p); e != nil {
			return e
		}
	}
	relation, evidence := transferAnswer(draft)
	return a.Jobs.ResolveTransfer(ctx, check.JobID, check.Key, id, relation, evidence)
}
