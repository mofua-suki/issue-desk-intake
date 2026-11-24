package issueapi

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

const replyPrefix = "reply-request:"

// Queue records live beside the existing idempotent comment action. They contain
// no credentials; recovery revalidates the sender before resuming that action.
func (s *Service) acceptReply(ctx context.Context, data Object, p auth.Principal) (Object, error) {
	id := fleet.Text(data["id"])
	if !uuid.MatchString(id) {
		return nil, fail(400, "送信IDが不正です。")
	}
	repo, n, err := s.target(ctx, data)
	if err != nil {
		return nil, err
	}
	if _, _, err = s.validateCommentRequest(data, "reply", p); err != nil {
		return nil, err
	}
	if p.Kind == auth.PC && s.o.Principal == nil {
		return nil, fail(503, "送り手を確認する処理を利用できません。")
	}
	input := Object{}
	for k, v := range data {
		if k != "async" {
			input[k] = v
		}
	}
	input["project"], input["number"] = repo, n
	delete(input, "desk")
	key := replyPrefix + id
	unlock := lock(&s.locks, key)
	defer unlock()
	saved, err := s.read(ctx, "actions", key)
	if err != nil {
		return nil, err
	}
	sender := ""
	if p.Kind == auth.PC {
		sender = "token:" + p.TokenID
	}
	if saved != nil {
		if saved["owner"] != sender || !sameReplyInput(fleet.ObjectOrEmpty(saved["input"]), input) {
			return nil, fail(409, "送信IDは別の返信で使用済みです。")
		}
	} else {
		saved = Object{"id": id, "owner": sender, "via": via(p), "kind": string(p.Kind), "input": input, "status": "queued", "createdAt": s.now(), "updatedAt": s.now()}
		if err = s.save(ctx, "actions", key, saved); err != nil {
			return nil, err
		}
	}
	s.launchReply(id)
	return replyView(saved), nil
}
func replyView(r Object) Object {
	input := fleet.ObjectOrEmpty(r["input"])
	return Object{"accepted": true, "id": r["id"], "project": input["project"], "number": input["number"], "action": input["action"], "createdAt": r["createdAt"], "status": r["status"], "result": r["result"], "error": r["error"], "errorCode": r["errorCode"], "updatedAt": r["updatedAt"]}
}
func (s *Service) replyRequest(ctx context.Context, id string, p auth.Principal) (Object, error) {
	if !uuid.MatchString(id) {
		return nil, fail(400, "送信IDが不正です。")
	}
	r, err := s.read(ctx, "actions", replyPrefix+id)
	if err != nil {
		return nil, err
	}
	if r == nil || p.Kind == auth.PC && r["owner"] != "token:"+p.TokenID {
		return nil, fail(404, "返信の依頼が見つかりません。")
	}
	if err = s.o.Project(fleet.Text(fleet.ObjectOrEmpty(r["input"])["project"])); err != nil {
		return nil, err
	}
	return replyView(r), nil
}
func (s *Service) launchReply(id string) {
	s.closing.Lock()
	defer s.closing.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	key := replyPrefix + id
	if _, running := s.closeInFlight.LoadOrStore(key, true); running {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.closeInFlight.Delete(key)
		unlock := lock(&s.locks, key+":run")
		defer unlock()
		r, err := s.read(s.ctx, "actions", key)
		if err != nil || r == nil || r["status"] != "queued" {
			return
		}
		p := auth.Principal{Kind: auth.Screen}
		if r["kind"] == string(auth.CLI) {
			p.Kind = auth.CLI
		}
		if owner := fleet.Text(r["owner"]); owner != "" {
			if s.o.Principal == nil {
				err = fail(503, "送り手を確認できません。")
			} else {
				p, err = s.o.Principal(s.ctx, owner)
			}
			if err == nil && (p.Kind != auth.PC || !p.Delegated || owner != "token:"+p.TokenID) {
				err = fail(403, "運用の代行を許した送り手ではありません。")
			}
		}
		var result Object
		if err == nil {
			result, err = s.commentWithVia(s.ctx, fleet.ObjectOrEmpty(r["input"]), p, fleet.Text(r["via"]))
		}
		if s.ctx.Err() != nil {
			return
		} // Keep queued for restart reconciliation.
		r["status"], r["result"], r["updatedAt"] = "done", result, s.now()
		delete(r, "error")
		delete(r, "errorCode")
		if err != nil {
			var problem *contract.Problem
			r["status"] = "queued" // Retry uncertain outcomes with the existing action ID.
			if errors.As(err, &problem) {
				r["error"], r["errorCode"] = problem.Message, problem.Code
				if problem.Status >= 400 && problem.Status < 500 && problem.Status != 408 && problem.Status != 429 {
					action, readErr := s.read(s.ctx, "actions", id)
					// A rejection after a possible external write is still an
					// uncertain outcome. Never allow a new ID to bypass it.
					if readErr == nil && action["attempted"] != true && action["comment"] == nil && action["postedComment"] == nil {
						r["status"] = "failed"
					}
				}
			} else {
				r["error"] = "返信を完了できません。送信結果は未確認です。"
			}
		}
		if err = s.save(s.ctx, "actions", key, r); err != nil {
			contract.LogFailure("返信結果の保存", err)
		}
	}()
}
func (s *Service) recoverReplies(ctx context.Context) error {
	rows, err := s.o.AppDB.QueryContext(ctx, "SELECT json FROM actions WHERE id LIKE 'reply-request:%'")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		r := Object{}
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return err
		}
		if r["status"] == "queued" && uuid.MatchString(fleet.Text(r["id"])) {
			ids = append(ids, fleet.Text(r["id"]))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.launchReply(id)
	}
	return nil
}

func sameReplyInput(a, b Object) bool {
	left, e := json.Marshal(a)
	right, f := json.Marshal(b)
	return e == nil && f == nil && string(left) == string(right)
}
