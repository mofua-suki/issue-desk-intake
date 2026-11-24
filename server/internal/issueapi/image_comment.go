package issueapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"issue-desk-intake/server/internal/attachments"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
)

func (s *Service) imageComment(ctx context.Context, repo string, n int64, data Object, p auth.Principal) (Object, error) {
	if !oneOf(fleet.Text(data["action"]), "reply", "resume", "retry") || data["fromBase"] == true || data["retest"] == true || data["autoRetest"] == true || data["overLimitStatement"] != nil || data["implementationLimitRetry"] == true {
		return nil, fail(400, "この操作には画像を添付できません。")
	}
	id := fleet.Text(data["id"])
	unlock := lock(&s.locks, "operation:"+id)
	defer unlock()
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	if !uuid.MatchString(fleet.Text(data["messageId"])) || len(fleet.Items(data["images"])) > 4 {
		return nil, fail(400, "画像のメッセージIDと4枚以内の画像を指定してください。")
	}
	ids, e := imageCommentIDs(data)
	if e != nil {
		return nil, e
	}
	payload := Object{"repo": repo, "number": n, "action": data["action"], "text": data["text"], "images": data["images"], "messageId": data["messageId"], "via": via(p)}
	saved, e := s.claimImageAction(ctx, id, payload)
	if e != nil {
		return nil, e
	}
	save := func() error { return s.save(ctx, "actions", id, saved) }
	if saved["applied"] == true {
		return Object{"id": fleet.ObjectOrEmpty(saved["comment"])["id"], "job": saved["job"]}, nil
	}
	if s.o.Attachments == nil || s.o.Answer == nil {
		return nil, fail(503, "画像の確認と転送を利用できません。")
	}
	selected, e := s.o.Attachments.Selected(ctx, fmt.Sprintf("issue:%s#%d", repo, n), ids, fleet.Text(data["messageId"]))
	if e != nil {
		return nil, e
	}
	detail, e := s.imageCommentDetail(ctx, repo, n, data, saved["attempted"] == true)
	if e != nil {
		return nil, e
	}
	if saved["imageReviews"] == nil {
		if e = s.reviewCommentImages(ctx, repo, detail, data, selected, saved, save); e != nil {
			return nil, e
		}
	}
	api, ok := s.o.Gitea.(attachments.API)
	if !ok {
		return nil, fail(503, "Giteaの画像転送を利用できません。")
	}
	if e = api.CheckAttachmentSupport(ctx); e != nil {
		return nil, e
	}
	marker := "<!-- issue-desk-action:" + id + " -->"
	initial := fleet.Text(data["text"]) + "\n\n" + marker + "\n" + attachments.Pending
	if saved["postedComment"] == nil {
		if e = s.postImagePlaceholder(ctx, repo, n, initial, selected, saved, save); e != nil {
			return nil, e
		}
	}
	if e = save(); e != nil {
		return nil, e
	}
	commentID, _ := number(fleet.ObjectOrEmpty(saved["postedComment"])["id"])
	if commentID < 1 {
		return nil, fail(503, "返信先コメントを確認できません。")
	}
	links, e := s.o.Attachments.Transfer(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d/assets", repo, commentID), selected, saved, save)
	if e != nil {
		return nil, e
	}
	instruction := fleet.Object(saved["instruction"])
	if instruction == nil {
		text, contextText, e := s.imageCommentText(ctx, fleet.Text(data["text"]), attachments.Description(selected, fleet.Items(saved["imageReviews"]), links))
		if e != nil {
			return nil, e
		}
		instruction = Object{"text": text, "context": contextText}
		saved["instruction"] = instruction
		if e = save(); e != nil {
			return nil, e
		}
	}
	text, contextText := fleet.Text(instruction["text"]), fleet.Text(instruction["context"])
	if saved["comment"] == nil {
		if e = s.finishImagePlaceholder(ctx, repo, commentID, initial, text+contextText+"\n\n"+marker); e != nil {
			return nil, e
		}
		saved["comment"] = Object{"id": commentID}
		saved["body"] = text + contextText
		if e = save(); e != nil {
			return nil, e
		}
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if e = s.controlImageCommentJob(ctx, job, id, fleet.Text(data["action"]), via(p), text, contextText, saved); e != nil {
		return nil, e
	}
	saved["applied"] = true
	saved["at"] = s.now()
	if e = save(); e != nil {
		return nil, e
	}
	s.emit(repo, n)
	return Object{"id": commentID, "job": saved["job"]}, nil
}

// finishImagePlaceholder reconciles a lost PATCH response and keeps edits made
// while attachments were uploaded. Gitea's REST API cannot require a version
// for this update, so an edit racing the final read and PATCH can still conflict.
func (s *Service) finishImagePlaceholder(ctx context.Context, repo string, id int64, initial, body string) error {
	route := fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id)
	var current Object
	if err := s.o.Gitea.Do(ctx, "GET", route, nil, &current); err != nil {
		return err
	}
	commentID, _ := number(current["id"])
	if commentID != id || !s.o.Own(fleet.ObjectOrEmpty(current["user"])) {
		return fail(409, "画像付き返信の投稿を照合できません。")
	}
	switch current["body"] {
	case body:
		return nil
	case initial:
		return s.o.Gitea.Do(ctx, "PATCH", route, Object{"body": body}, nil)
	default:
		return fail(409, "画像付き返信の仮本文が変更されたため、編集内容を保持しました。")
	}
}

// claimImageAction は送信IDの保存済みの記録を読み、同じ操作の続きか未使用なら記録を返す。別の操作で使用済みなら断る。
func (s *Service) claimImageAction(ctx context.Context, id string, payload Object) (Object, error) {
	saved, e := s.read(ctx, "actions", id)
	if e != nil {
		return nil, e
	}
	if saved != nil && !samePayload(fleet.ObjectOrEmpty(saved["payload"]), payload) {
		return nil, fail(409, "送信IDは別の操作で使用済みです。")
	}
	if saved == nil {
		saved = Object{"payload": payload, "applied": false}
	}
	return saved, nil
}

// imageCommentDetail は使用量の上限と依頼の状態から画像付きの操作を受け付けるかを判定し、依頼の詳細を返す。
// attempted は同じ送信IDで既に送り始めたかで、その続きは状態による断りを飛ばす。
func (s *Service) imageCommentDetail(ctx context.Context, repo string, n int64, data Object, attempted bool) (Object, error) {
	if oneOf(fleet.Text(data["action"]), "reply", "retry") {
		job, e := s.o.Jobs.ByIssue(ctx, repo, n)
		if e != nil {
			return nil, e
		}
		if s.o.Jobs.UsageLimitHeld(ctx, job, jobs.ControlOptions{UsageLimit: s.o.UsageLimit, UsageWeights: s.o.UsageWeights}) {
			return nil, fail(409, "累計使用量の上限を超えています。画像を付けずに承認付き再試行を使ってください。")
		}
	}
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return nil, e
	}
	if !attempted {
		action := fleet.Text(data["action"])
		if action == "retry" && detail["status"] == "review_held" {
			action = "review"
		}
		if reason := refusal(detail, action); reason != "" {
			return nil, fail(409, reason)
		}
	}
	return detail, nil
}

// imageCommentText は本文に参照した依頼の解決結果と画像の説明を足し、8000文字に収まらない分を補足に分けて返す。
func (s *Service) imageCommentText(ctx context.Context, text, description string) (string, string, error) {
	refs, e := s.references(ctx, text)
	if e != nil {
		return "", "", e
	}
	contextText := ""
	if contract.UTF16Length(text+refs+description) <= 8000 {
		text += refs + description
	} else {
		contextText = refs + description
	}
	return text, contextText, nil
}

// imageCommentIDs は添付する画像の指定が重複のない送信IDの並びかを検査して返す。
func imageCommentIDs(data Object) ([]string, error) {
	ids := []string{}
	seen := map[string]bool{}
	for _, v := range fleet.Items(data["images"]) {
		id := fleet.Text(v)
		if !uuid.MatchString(id) || seen[id] {
			return nil, fail(400, "画像の指定が重複または不正です。")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// reviewCommentImages は添付する画像を受付AIに確かめさせ、未確定事項が無いときだけ確認結果を記録する。
func (s *Service) reviewCommentImages(ctx context.Context, repo string, detail, data Object, selected []attachments.Image, saved Object, save func() error) error {
	images := []any{}
	for _, im := range selected {
		images = append(images, Object{"id": im.ID, "mime": im.MIME, "data": base64.StdEncoding.EncodeToString(im.Bytes)})
	}
	answer, e := s.o.Answer(ctx, Object{"kind": "image-comment", "project": repo, "title": detail["title"], "body": detail["body"], "comments": detail["comments"], "text": data["text"], "images": images})
	if e != nil {
		return e
	}
	reviews := fleet.Items(answer["imageReviews"])
	if e = attachments.ValidateReviews(selected, reviews); e != nil {
		return e
	}
	if answer["ready"] != true {
		return fail(409, "画像付き追加指示には確認が必要です。")
	}
	for _, v := range reviews {
		if strings.TrimSpace(fleet.Text(fleet.ObjectOrEmpty(v)["uncertainty"])) != "" {
			return fail(409, "画像の未確定事項を確認してください。")
		}
	}
	saved["imageReviews"] = reviews
	if e = save(); e != nil {
		return e
	}
	return nil
}

// postImagePlaceholder は画像を転送する前の仮の返信を1回だけ投稿する。既に同じ本文があればそれを記録し、結果を照合できない再投稿は断る。
func (s *Service) postImagePlaceholder(ctx context.Context, repo string, n int64, initial string, selected []attachments.Image, saved Object, save func() error) error {
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return e
	}
	for _, comment := range comments {
		if s.o.Own(fleet.ObjectOrEmpty(comment["user"])) && comment["body"] == initial {
			saved["postedComment"] = Object{"id": comment["id"]}
			break
		}
	}
	if saved["postedComment"] == nil {
		if saved["attempted"] == true {
			return fail(409, "前回の画像付き返信結果を照合できません。再投稿しません。")
		}
		saved["attempted"] = true
		if e = save(); e != nil {
			return e
		}
		if e = s.o.Attachments.Bind(ctx, selected); e != nil {
			return e
		}
		comment, e := s.o.Gitea.Comment(ctx, repo, n, initial)
		if e != nil {
			if explicitRejection(e) {
				saved["attempted"] = false
				_ = save()
			}
			return e
		}
		saved["postedComment"] = Object{"id": comment["id"]}
	}
	return nil
}

// controlImageCommentJob は画像付きの指示をジョブへの操作として送り、変わったジョブを記録する。
func (s *Service) controlImageCommentJob(ctx context.Context, job *jobs.Job, id, requested, by, text, contextText string, saved Object) error {
	if job != nil && !oneOf(job.Status, "merged", "closed") {
		action := requested
		if action == "reply" {
			action = "instruction"
			if job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "review_held" {
				action = "retry"
			}
		}
		changed, e := s.o.Jobs.Control(ctx, job.ID, jobs.ControlInput{ID: id, Action: action, By: by, Text: text, Context: contextText}, jobs.ControlOptions{RunLimit: s.o.RunLimit, UsageLimit: s.o.UsageLimit, UsageWeights: s.o.UsageWeights})
		if e != nil {
			return e
		}
		saved["job"] = changed.ID
	} else if requested != "reply" {
		return fail(409, "この依頼に操作できるジョブがありません。")
	}
	return nil
}
