package dispatch

import (
	"context"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

// applyPreapproval は、マージ依頼（merge_manual）の approvalRequest が指す作成依頼（pr_create）の事前承認を確かめ、
// 合えば承認発言・指示日時・運用代行の印をマージ依頼の payload へ写す。PR番号が決まる前の承認を、
// そのPRを作った依頼の結果（同じrepo・送り手・PR番号・head）に結び付けて使うためのもの。
// 作成依頼の approvalDelegated は、運用代行を許したトークンから承認が届いたときだけ受付が付けた印。
func applyPreapproval(ctx context.Context, q queryer, request *Request) error {
	id := fleet.Text(request.Payload["approvalRequest"])
	if request.Kind != "merge_manual" || id == "" {
		return nil
	}
	source, err := readRequest(ctx, q, id)
	if err != nil {
		return err
	}
	if source == nil || source.Kind != "pr_create" || source.Repo != request.Repo || source.Sender != request.Sender {
		return problem(400, "approvalRequestの作成依頼が、同じ送り手・リポジトリのPR作成依頼として見つかりません。")
	}
	if source.Status != "done" || fleet.Text(source.Result["status"]) != "created" {
		return problem(400, "approvalRequestの作成依頼はPRの作成が完了していません。")
	}
	number, _ := source.Result["number"].(float64)
	if number != request.Payload["number"] || fleet.Text(source.Result["head"]) != fleet.Text(request.Payload["head"]) {
		return problem(400, "approvalRequestの作成依頼で作ったPRの番号・headが、マージ依頼のnumber・headと違います。")
	}
	approval := strings.TrimSpace(fleet.Text(source.Payload["approval"]))
	if approval == "" || source.Payload["approvalDelegated"] != true || fleet.Text(source.Payload["instructionAt"]) == "" {
		return problem(400, "approvalRequestの作成依頼に、運用代行を許したトークンから送った承認発言（approval・instructionAt）がありません。")
	}
	// 直接の承認と同じく、指示日時が未来でなく7日以内であることを、マージ依頼を受け付ける時点で確かめ直す。
	at, ok := contract.InstructionAt(source.Payload["instructionAt"], now())
	if !ok {
		return problem(400, "approvalRequestの作成依頼の承認は期限（指示から7日）を過ぎています。新しい承認を付けてください。")
	}
	request.Payload["approval"] = approval
	request.Payload["instructionAt"] = at
	request.Payload["approvalDelegated"] = true
	return nil
}
