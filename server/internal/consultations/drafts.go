package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"math/big"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/models"
)

// requireTier refuses a new consultation whose requester did not choose the
// consultation tier. The intake no longer judges a tier by itself, so every
// path that starts a consultation (screen, script, TODO, internal) must send it.
func requireTier(value any) error {
	if value == nil || value == "" {
		return contract.NewProblem(400, models.TierRequiredMessage, "TIER_REQUIRED")
	}
	if value != "light" && value != "standard" && value != "heavy" {
		return refuse(400, "難易度はlight・standard・heavyで指定してください。")
	}
	return nil
}

func (s *Service) draftAccess(d Record, p auth.Principal) error {
	if d == nil || p.Kind == auth.PC && !p.Delegated && text(d["owner"]) != owner(p) {
		return refuse(404, "下書きが見つかりません。")
	}
	if d["unassigned"] == true && d["project"] == nil {
		return nil
	}
	return s.checkProject(text(d["project"]))
}
func (s *Service) Draft(ctx context.Context, id string, p auth.Principal) (Record, error) {
	d, _, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		todo, found, err := s.options.Records.Get(ctx, "todos", id)
		if err != nil {
			return nil, err
		}
		if found && todo["returnedAt"] != nil {
			return nil, contract.NewProblem(409, "この相談はTODOに戻しました。TODOから続けてください。", "draft_returned_to_todo")
		}
	}
	if err := s.draftAccess(d, p); err != nil {
		return nil, err
	}
	return d, nil
}
func (s *Service) CreateDraft(ctx context.Context, data Record, p auth.Principal, script bool) (Record, error) {
	return s.createDraft(ctx, data, p, script, false)
}
func (s *Service) createDraft(ctx context.Context, data Record, p auth.Principal, script, checkArchive bool) (Record, error) {
	request, err := s.draftCreationRequest(ctx, data, script, checkArchive)
	if err != nil {
		return nil, err
	}
	return s.options.Records.Change(ctx, "drafts", request.id, func(tx *sql.Tx, current Record) (Record, error) {
		if current != nil {
			if err := existingDraftConflict(current, request, data, p, script); err != nil {
				return nil, err
			}
			return current, nil
		}
		if err := checkDraftIDReusable(ctx, tx, request.id); err != nil {
			return nil, err
		}
		return s.newDraftRecord(request, data, p, script)
	})
}

// draftCreation is the validated request to create a draft.
type draftCreation struct {
	id, provider, consultationProvider string
	unassigned                         bool
	project, priority                  any
}

// draftCreationRequest validates the creation request and resolves its provider and ID.
func (s *Service) draftCreationRequest(ctx context.Context, data Record, script, checkArchive bool) (draftCreation, error) {
	unassigned := data["unassigned"] == true && (data["project"] == nil || data["project"] == "")
	if data["unassigned"] != nil && data["unassigned"] != false && data["unassigned"] != true {
		return draftCreation{}, refuse(400, "unassignedは真偽値で指定してください。")
	}
	if data["unassigned"] == true && !unassigned {
		return draftCreation{}, refuse(400, "プロジェクト未定ではプロジェクトを指定できません。")
	}
	if !unassigned {
		if err := s.checkProject(text(data["project"])); err != nil {
			return draftCreation{}, err
		}
	}
	provider, err := s.provider(text(data["provider"]))
	if err != nil {
		return draftCreation{}, err
	}
	cp, err := consultationProvider(text(data["consultationProvider"]))
	if err != nil {
		return draftCreation{}, err
	}
	id := text(data["id"])
	if id == "" {
		id, err = newID()
		if err != nil {
			return draftCreation{}, err
		}
	}
	if !recordID.MatchString(id) {
		return draftCreation{}, refuse(400, "下書きIDが不正です。")
	}
	if err := requireTier(data["tier"]); err != nil {
		return draftCreation{}, err
	}
	if value := data["implementationTier"]; value != nil && value != "light" && value != "standard" && value != "heavy" {
		return draftCreation{}, refuse(400, "難易度はlight・standard・heavyで指定してください。")
	}
	if value := data["hold"]; value != nil && value != true && value != false {
		return draftCreation{}, refuse(400, "holdは真偽値で指定してください。")
	}
	priority := data["priority"]
	if priority != nil && priority != "high" && priority != "normal" {
		return draftCreation{}, refuse(400, "優先度はhighかnormalを指定してください。")
	}
	if script && priority == "high" && (strings.TrimSpace(text(data["priorityReason"])) == "" || len([]rune(strings.TrimSpace(text(data["priorityReason"])))) > 500) {
		return draftCreation{}, refuse(400, "割り込みの理由を1〜500文字で指定してください。")
	}
	if checkArchive && !unassigned {
		if _, err := s.CheckRepositoryArchive(ctx, text(data["project"])); err != nil {
			return draftCreation{}, err
		}
	}
	project := data["project"]
	if unassigned {
		project = nil
	}
	return draftCreation{id: id, provider: provider, consultationProvider: cp, unassigned: unassigned, project: project, priority: priority}, nil
}

// existingDraftConflict refuses a repeated creation whose ID belongs to a different request.
func existingDraftConflict(current Record, request draftCreation, data Record, p auth.Principal, script bool) error {
	oldCP := text(current["consultationProvider"])
	if oldCP == "" {
		oldCP = "codex"
	}
	mismatch := current["project"] != request.project || text(current["provider"]) != request.provider || oldCP != request.consultationProvider || current["priority"] != request.priority || current["requestedTier"] != data["tier"] || current["requestedImplementationTier"] != data["implementationTier"] || (current["hold"] == true) != (data["hold"] == true) || (current["unassigned"] == true) != request.unassigned
	if mismatch || owner(p) != "" && text(current["owner"]) != owner(p) {
		return refuse(409, "下書きIDは別の依頼で使用済みです。")
	}
	if script {
		left, _ := canonicalJSON(current["replaceIssues"])
		right, _ := canonicalJSON(data["replaceIssues"])
		if left != right {
			return refuse(409, "下書きIDは別の置き換え依頼で使用済みです。")
		}
	}
	return nil
}

// checkDraftIDReusable refuses an ID that is held by a receipt or by a TODO returned from a draft.
func checkDraftIDReusable(ctx context.Context, tx *sql.Tx, id string) error {
	if err := receiptDraftAvailable(ctx, tx, id); err != nil {
		return err
	}
	var raw string
	err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM todos WHERE id=?"), id).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		todo, err := decode(raw)
		if err != nil {
			return err
		}
		if todo["returnedAt"] != nil {
			return refuse(409, "この相談はTODOに戻しています。")
		}
	}
	return nil
}

// newDraftRecord builds the stored record of a new draft.
func (s *Service) newDraftRecord(request draftCreation, data Record, p auth.Principal, script bool) (Record, error) {
	id, project, provider, cp, unassigned, priority := request.id, request.project, request.provider, request.consultationProvider, request.unassigned, request.priority
	now := timestamp(s.options.Now())
	tier, err := s.options.InitialTier(cp, text(data["tier"]), s.options.Now())
	if err != nil {
		return nil, err
	}
	d := Record{"id": id, "project": project, "provider": provider, "consultationProvider": cp, "status": "draft", "questions": []any{}, "messages": []any{}, "title": "", "body": "", "ready": false, "revision": 0, "createdAt": now, "updatedAt": now, "tier": tier, "completionPlanVersion": 1}
	if unassigned {
		d["unassigned"] = true
	}
	if owner(p) != "" {
		d["owner"] = owner(p)
	}
	if data["hold"] == true {
		d["hold"] = true
	}
	if priority != nil {
		d["priority"] = priority
		via := "screen"
		if script {
			via = "script"
		}
		source := Record{"via": via, "at": now}
		if p.Name != "" {
			source["pc"] = p.Name
		}
		if script && priority == "high" {
			source["reason"] = strings.TrimSpace(text(data["priorityReason"]))
		}
		d["prioritySource"] = source
	}
	if data["tier"] != nil {
		d["requestedTier"] = data["tier"]
	}
	if data["implementationTier"] != nil {
		d["requestedImplementationTier"] = data["implementationTier"]
	}
	if script {
		for _, key := range []string{"replaceIssues", "notDuplicate", "despitePending", "duplicateHints", "closedHistoryDesks", "readEvidence"} {
			if value, ok := data[key]; ok {
				d[key] = value
			}
		}
	}
	return d, nil
}

func (s *Service) Chat(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	engine := s.engineSnapshot()
	if engine.Start == nil {
		return nil, refuse(503, "相談の実行先を利用できません。")
	}
	retry := data["retry"] == true
	suppliedID := text(data["messageId"])
	requestID := text(data["id"])
	changed, err := s.options.Records.Change(ctx, "drafts", id, func(tx *sql.Tx, d Record) (Record, error) {
		if err := s.draftAccess(d, p); err != nil {
			return nil, err
		}
		if value, exists := data["expectedRevision"]; exists {
			n, ok := chatRevision(value)
			if !ok || n < 0 || n > 9007199254740991 {
				return nil, refuse(400, "expectedRevisionは0以上の安全な整数で指定してください。")
			}
		}
		body, valid := data["text"].(string)
		body = strings.TrimSpace(body)
		if !retry {
			if repeated, err := repeatedChatMessage(d, data, suppliedID, requestID, body, valid); err != nil {
				return nil, err
			} else if repeated {
				return d, nil
			}
		}
		if !retry {
			if value, exists := data["expectedRevision"]; exists {
				expected, _ := chatRevision(value)
				current, ok := integer(d["revision"])
				if !ok || expected != current {
					return nil, refuse(409, "会話・質問を読み直して回答してください。")
				}
			}
		}
		action := "chat"
		if retry {
			action = "retry"
		}
		if refusal := s.refusal(d, action); refusal != "" {
			return nil, refuse(409, refusal)
		}
		if value := data["tier"]; value != nil && value != "light" && value != "standard" && value != "heavy" {
			return nil, refuse(400, "難易度はlight・standard・heavyで指定してください。")
		}
		next := clone(d)
		now := timestamp(s.options.Now())
		if !retry {
			if value, exists := data["id"]; exists && (!recordID.MatchString(requestID) || value == nil) {
				return nil, refuse(400, "送信IDが不正です。")
			}
			if value, exists := data["messageId"]; exists && (!recordID.MatchString(suppliedID) || value == nil) {
				return nil, refuse(400, "発言IDが不正です。")
			}
			if err := s.appendChatMessage(ctx, tx, id, d, next, data, p, body, valid, suppliedID, requestID, now); err != nil {
				return nil, err
			}
		}
		queueChatTurn(next, d, retry, now)
		if tier := data["tier"]; tier != nil {
			changed, err := s.options.ChangeTier(next, text(tier), s.options.Now())
			if err != nil {
				return nil, err
			}
			next["tier"] = changed
		}
		return next, nil
	})
	if err != nil {
		if _, supplied := data["expectedRevision"]; supplied && !retry && problem(err).Status == 409 && problem(err).Message == "別の操作で内容が更新されています。" {
			return nil, refuse(409, "会話・質問を読み直して回答してください。")
		}
		return nil, err
	}
	if text(changed["status"]) == "thinking" {
		return engine.Start(ctx, id, changed, p)
	}
	return changed, nil
}

// repeatedChatMessage reports whether the send ID names a message already saved with
// the same content, and refuses a send ID reused for different content.
func repeatedChatMessage(d, data Record, suppliedID, requestID, body string, valid bool) (bool, error) {
	if suppliedID == "" && requestID == "" {
		return false, nil
	}
	for _, entry := range fleet.Items(d["messages"]) {
		m := record(entry)
		if suppliedID != "" && text(m["id"]) == suppliedID || requestID != "" && text(m["requestId"]) == requestID {
			if !valid || text(m["role"]) != "user" || text(m["text"]) != body || suppliedID != "" && text(m["id"]) != suppliedID {
				return false, refuse(409, "送信IDは別のメッセージで使用済みです。")
			}
			actual := []any{}
			for _, image := range fleet.Items(m["images"]) {
				actual = append(actual, record(image)["id"])
			}
			expected := fleet.Items(data["images"])
			if expected == nil {
				expected = []any{}
			}
			left, _ := canonicalJSON(actual)
			right, _ := canonicalJSON(expected)
			if left != right {
				return false, refuse(409, "送信IDは別の画像で使用済みです。")
			}
			return true, nil
		}
	}
	return false, nil
}

// appendChatMessage validates the user's message, binds its images and appends it to the conversation.
func (s *Service) appendChatMessage(ctx context.Context, tx *sql.Tx, id string, d, next, data Record, p auth.Principal, body string, valid bool, suppliedID, requestID, now string) error {
	original, _ := data["text"].(string)
	if !valid || contract.UTF16Length(body) < 1 || contract.UTF16Length(original) > 8000 {
		return refuse(400, "内容を1〜8000文字で入力してください。")
	}
	messageID := suppliedID
	if messageID == "" {
		var err error
		messageID, err = newID()
		if err != nil {
			return err
		}
	}
	images, err := s.draftImages(ctx, tx, id, messageID, data, p)
	if err != nil {
		return err
	}
	receipt, err := groupExceptionReceipt(d, data, p, s.options.Now())
	if err != nil {
		return err
	}
	message := Record{"id": messageID, "role": "user", "text": body, "at": now, "images": images, "source": string(p.Kind)}
	if receipt != nil {
		message["groupExceptionApproval"] = receipt
	}
	if requestID != "" {
		message["requestId"] = requestID
	}
	messages := append(append([]any{}, fleet.Items(d["messages"])...), message)
	if _, err := projectConversation(messages, 80000); err != nil {
		var shortfall *ProjectionShortfall
		if errors.As(err, &shortfall) {
			return refuse(400, "会話全体の上限を超えています。")
		}
		return err
	}
	if err := recordReply(ctx, tx, d, message); err != nil {
		return err
	}
	next["messages"], next["continuation"] = messages, 0
	releaseUnmeasuredHold(next, messageID, now)
	return nil
}

// queueChatTurn returns the draft to the AI queue and carries the read context into the next turn.
func queueChatTurn(next, d Record, retry bool, now string) {
	delete(next, "submissionRequest")
	revision, _ := integer(d["revision"])
	next["revision"], next["updatedAt"], next["status"], next["ready"], next["queued"] = revision+1, now, "thinking", false, true
	SetDraftError(next, nil, nil)
	carried := d["context"]
	if d["carriedContext"] != nil {
		carried = d["carriedContext"]
	}
	if d["progress"] != nil {
		carried = d["progress"]
	}
	source := "accepted"
	if retry && d["status"] == "stalled" {
		source = "resumed"
		delete(next, "stallCause")
		if d["stalled"] != nil {
			next["stallHistory"] = append(fleet.Items(d["stallHistory"]), d["stalled"])
		}
	}
	for key, value := range acceptance(now, source) {
		next[key] = value
	}
	next["carriedContext"], next["stalled"] = carried, nil
}

func record(value any) Record { return fleet.Object(value) }
func (s *Service) draftImages(ctx context.Context, tx *sql.Tx, id, messageID string, data Record, p auth.Principal) ([]Record, error) {
	ids := fleet.Items(data["images"])
	if len(ids) > 4 {
		return nil, refuse(400, "1つの発言には画像を4枚まで添付できます。")
	}
	if p.Kind == auth.PC && len(ids) > 0 {
		return nil, refuse(400, "トークン接続では画像を添付できません。")
	}
	seen := map[string]bool{}
	result := []Record{}
	for _, value := range ids {
		image := text(value)
		if !recordID.MatchString(image) || seen[image] {
			return nil, refuse(400, "画像の指定が不正です。")
		}
		seen[image] = true
		var raw, scope, message string
		var bound int
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json,scope,message,bound FROM images WHERE id=?"), image).Scan(&raw, &scope, &message, &bound)
		if err == sql.ErrNoRows {
			return nil, refuse(400, "添付画像がありません。")
		}
		if err != nil {
			return nil, err
		}
		if scope != "draft:"+id || message != messageID || bound != 0 {
			return nil, refuse(403, "別の依頼・発言の画像は使用できません。")
		}
		meta, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, meta)
		if _, err := tx.ExecContext(ctx, bindSQL("UPDATE images SET bound=1 WHERE id=?"), image); err != nil {
			return nil, err
		}
	}
	var existing int
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM images WHERE scope=? AND bound=1"), "draft:"+id).Scan(&existing); err != nil {
		return nil, err
	}
	if existing > 12 {
		return nil, refuse(400, "相談には画像を12枚まで添付できます。")
	}
	return result, nil
}

func chatRevision(value any) (int64, bool) {
	if n, ok := value.(json.Number); ok {
		raw := string(n)
		if len(raw) > 128 {
			return 0, false
		}
		if index := strings.IndexAny(raw, "eE"); index >= 0 {
			exponent, err := strconv.Atoi(raw[index+1:])
			if err != nil || exponent < -128 || exponent > 128 {
				return 0, false
			}
		}
		r, valid := new(big.Rat).SetString(string(n))
		if !valid || !r.IsInt() || !r.Num().IsInt64() {
			return 0, false
		}
		return r.Num().Int64(), true
	}
	return integer(value)
}
