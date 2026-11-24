package consultations

// 相談の編集・優先度の変更・確定（起票）の業務の操作。

import (
	"context"
	"database/sql"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func (s *Service) EditDraft(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	return s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if reason := s.refusal(d, "edit"); reason != "" {
			return nil, refuse(409, reason)
		}
		expected, ok := integer(data["revision"])
		actual, _ := integer(d["revision"])
		if !ok || expected != actual {
			return nil, refuse(409, "下書きは更新されています。")
		}
		title, body := text(data["title"]), text(data["body"])
		if strings.TrimSpace(title) == "" || contract.UTF16Length(title) > 200 || strings.TrimSpace(body) == "" || contract.UTF16Length(body) > 20000 {
			return nil, refuse(400, "タイトルと本文を確認してください。")
		}
		provider := text(d["provider"])
		if requested := text(data["provider"]); requested != "" {
			provider = requested
		}
		provider, err := s.provider(provider)
		if err != nil {
			return nil, err
		}
		next := clone(d)
		next["title"], next["body"], next["provider"], next["revision"], next["updatedAt"] = strings.TrimSpace(title), strings.TrimSpace(body), provider, actual+1, timestamp(s.options.Now())
		return next, nil
	})
}

func (s *Service) SetDraftPriority(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	value := text(data["priority"])
	if !is(value, "high", "normal", "auto") {
		return nil, refuse(400, "優先度はhigh・normal・autoで指定してください。")
	}
	changed := false
	draft, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if has(d, "consultationChildren") || d["parentSplit"] != nil || has(d, "splitChildren") || d["proposal"] != nil || d["issue"] != nil || is(text(d["status"]), "project_handoff", "auto_ready", "submitting", "submitted", "submit_unknown") {
			return nil, refuse(409, "この相談の優先度は変更できません。")
		}
		current := text(d["priority"])
		if current == "" {
			current = "auto"
		}
		if value == current {
			return d, nil
		}
		changed = true
		next := clone(d)
		if value == "auto" {
			delete(next, "priority")
			delete(next, "prioritySource")
		} else {
			next["priority"], next["prioritySource"] = value, Record{"via": "draft-screen", "at": timestamp(s.options.Now())}
		}
		next["updatedAt"] = timestamp(s.options.Now())
		return next, nil
	})
	if err == nil && changed && s.options.PublishDraft != nil {
		s.options.PublishDraft(Record{"type": "draft", "id": id})
	}
	return draft, err
}

// AcceptSubmission persists the intent only. Engine-owned filing and the
// recovery cycle reconcile it independently of the HTTP request.
func (s *Service) AcceptSubmission(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	if s.options.Submit == nil {
		return nil, refuse(503, "起票の処理を利用できません。")
	}
	d, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, current Record) (Record, error) {
		if err := s.draftAccess(current, p); err != nil {
			return nil, err
		}
		if p.Kind == auth.PC && !p.Delegated && current["hold"] != true {
			return nil, refuse(403, "相談だけのモードで始めた相談でないため、トークン接続では確定できません。")
		}
		if current["status"] == "submitted" && (current["issue"] != nil || has(current, "issues")) {
			return current, nil
		}
		requested, valid := integer(data["revision"])
		previous, repeated := integer(record(current["submissionRequest"])["revision"])
		if valid && repeated && requested == previous {
			return current, nil
		}
		if current["status"] == "auto_ready" && repeated {
			return nil, refuse(409, "起票を受付済みです。同じ下書きIDで結果を確認してください。")
		}
		actual, _ := integer(current["revision"])
		if !valid || requested != actual {
			return nil, refuse(409, "下書きは更新されています。")
		}
		action := "submit"
		if is(text(current["status"]), "submit_error", "submit_unknown") {
			action = "resubmit"
		}
		if reason := s.refusal(current, action); reason != "" {
			return nil, refuse(409, reason)
		}
		next := clone(current)
		next["submissionRequest"] = Record{"revision": requested, "at": timestamp(s.options.Now()), "owner": owner(p), "kind": string(p.Kind)}
		next["status"], next["revision"], next["updatedAt"] = "auto_ready", actual+1, timestamp(s.options.Now())
		if is(text(current["status"]), "submitting", "submit_unknown") {
			next["status"] = current["status"]
		}
		SetDraftError(next, nil, nil)
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	if s.options.SubmissionQueued != nil {
		s.options.SubmissionQueued(d)
	}
	view := clone(d)
	view["accepted"] = true
	return view, nil
}
