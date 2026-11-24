package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

var draftStopPolicy = auth.Policy{PC: true, CLI: true, Screen: true, RequireDelegation: true}

// Stop keeps the conversation and usage evidence, and fences late checkpoints
// before cancelling only this consultation's execution.
func (s *Service) Stop(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	if err := auth.Authorize(p, draftStopPolicy); err != nil {
		return nil, err
	}
	for key := range data {
		if !is(key, "operationId", "revision", "reason") {
			return nil, refuse(400, "停止にはoperationId・revision・reasonを指定してください。")
		}
	}
	operation, reason := text(data["operationId"]), strings.TrimSpace(text(data["reason"]))
	revision, valid := integer(data["revision"])
	if !recordID.MatchString(operation) || !valid || revision < 0 || reason == "" || contract.UTF16Length(reason) > 500 || strings.ContainsFunc(reason, func(r rune) bool { return r < 32 || r >= 127 && r <= 159 }) {
		return nil, refuse(400, "停止の操作ID・revision・1〜500文字の理由を指定してください。")
	}
	functions := s.engineSnapshot()
	if functions.StopExecution == nil {
		return nil, refuse(503, "相談の実行処理を停止できません。")
	}
	return functions.StopExecution(id, func() (Record, bool, error) {
		changed := false
		d, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
			if err := s.draftAccess(d, p); err != nil {
				return nil, err
			}
			for _, entry := range fleet.Items(d["consultationStops"]) {
				prior := record(entry)
				if prior["operationId"] == operation {
					before, _ := integer(prior["revision"])
					if before != revision || prior["reason"] != reason || prior["by"] != stopActor(p) {
						return nil, refuse(409, "停止の操作IDは別の内容で使用済みです。")
					}
					return d, nil
				}
			}
			if reason := draftRefusal(d, "stop", false); reason != "" {
				return nil, refuse(409, reason)
			}
			current, ok := integer(d["revision"])
			if !ok || current != revision {
				return nil, refuse(409, "相談の最新状態を読み直してから停止してください。")
			}
			next := clone(d)
			at := timestamp(s.options.Now())
			stop := Record{"operationId": operation, "revision": revision, "at": at, "by": stopActor(p), "reason": reason, "run": d["run"]}
			next["consultationStops"] = append(fleet.Items(d["consultationStops"]), stop)
			next["status"], next["ready"], next["queued"], next["run"], next["retryAt"] = "stalled", false, false, nil, nil
			next["stallCause"] = "explicit"
			next["revision"], next["updatedAt"] = current+1, at
			SetDraftError(next, "相談を停止しました: "+reason, nil)
			changed = true
			return next, nil
		})
		return d, changed, err
	})
}

func stopActor(p auth.Principal) string {
	if p.Kind == auth.PC {
		return owner(p)
	}
	return string(p.Kind)
}
