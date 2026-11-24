package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func (s *Service) create(ctx context.Context, data Object) (Object, error) {
	text, id, provider, consult, e := s.parseCreateRequest(data)
	if e != nil {
		return nil, e
	}
	if existing, found, e := s.existingProjectConsultation(ctx, id, text, provider, consult); e != nil || found {
		return existing, e
	}
	if e := s.consultationIDInUse(ctx, id); e != nil {
		return nil, e
	}
	p := Object{"id": id, "provider": provider, "consultationProvider": consult, "status": "draft", "messages": []any{Object{"id": id, "requestId": id, "role": "user", "text": text}}, "questions": []any{}, "definition": nil, "fingerprint": nil, "approval": nil, "revision": 0, "createdAt": s.now()}
	if e := s.think(ctx, p); e != nil {
		return nil, e
	}
	return s.present(p), nil
}

// parseCreateRequest は新しいプロジェクトの相談の本文・ID・実装AI・相談AIを検査し、既定値を補って返す。
func (s *Service) parseCreateRequest(data Object) (text, id, provider, consult string, err error) {
	if p := contract.OnlyKeys(data, "id", "text", "provider", "consultationProvider"); p != nil {
		return "", "", "", "", p
	}
	text = strings.TrimSpace(fleet.Text(data["text"]))
	if text == "" || size(text) > 8000 {
		return "", "", "", "", problem(400, "相談は8000文字以内で入力してください。")
	}
	id = fleet.Text(data["id"])
	if !uuidPattern.MatchString(id) {
		return "", "", "", "", problem(400, "相談IDが不正です。")
	}
	provider = fleet.Text(data["provider"])
	if provider == "" {
		provider = s.o.Provider
	}
	if provider != "codex" && provider != "claude" {
		return "", "", "", "", problem(400, "実装AIが不正です。")
	}
	consult = fleet.Text(data["consultationProvider"])
	if consult == "" {
		consult = "codex"
	}
	if consult != "codex" && consult != "claude" {
		return "", "", "", "", problem(400, "相談AIが不正です。")
	}
	return text, id, provider, consult, nil
}

// existingProjectConsultation は同じIDの相談が保存済みなら、同じ内容の送り直しかを照合して今の状態を返す。保存が無ければ found=false を返す。
func (s *Service) existingProjectConsultation(ctx context.Context, id, text, provider, consult string) (Object, bool, error) {
	if existing, e := s.load(ctx, id); e == nil {
		messages := fleet.List(existing["messages"])
		oldConsult := fleet.Text(existing["consultationProvider"])
		if oldConsult == "" {
			oldConsult = "codex"
		}
		if existing["provider"] != provider || oldConsult != consult || len(messages) == 0 || fleet.ObjectOrEmpty(messages[0])["text"] != text {
			return nil, false, problem(409, "同じIDで別の相談が保存されています。")
		}
		return s.present(existing), true, nil
	} else {
		var p *contract.Problem
		if !errors.As(e, &p) || p.Status != 404 {
			return nil, false, e
		}
	}
	return nil, false, nil
}

// consultationIDInUse は同じIDが相談・TODOの表で使われていれば断る。
func (s *Service) consultationIDInUse(ctx context.Context, id string) error {
	for _, drafts := range []bool{true, false} {
		var one int
		var e error
		if drafts {
			e = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT 1 FROM drafts WHERE id=?"), id).Scan(&one)
		} else {
			e = s.o.AppDB.QueryRowContext(ctx, bindSQL("SELECT 1 FROM todos WHERE id=?"), id).Scan(&one)
		}
		if e == nil {
			return problem(409, "同じIDは別の相談で使われています。")
		}
		if e != sql.ErrNoRows && e != nil && !strings.Contains(e.Error(), "no such table") {
			return e
		}
	}
	return nil
}

func (s *Service) mutate(ctx context.Context, id, action string, data Object, principal auth.Principal) (Object, error) {
	p, e := s.load(ctx, id)
	if e != nil {
		return nil, e
	}
	switch action {
	case "chat":
		return s.chatProject(ctx, id, p, data)
	case "approve":
		return s.approveProject(ctx, p, data, principal)
	case "create":
		if q := contract.OnlyKeys(data); q != nil {
			return nil, q
		}
		e = s.startCreate(ctx, p)
	case "file":
		if q := contract.OnlyKeys(data); q != nil {
			return nil, q
		}
		if p["status"] != "registered" || fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["registered"] == nil {
			return nil, problem(409, "登録が済んでいない定義からは起票しません。")
		}
		if fleet.ObjectOrEmpty(p["filing"])["status"] == "host_waiting" {
			return nil, problem(409, "担当表の反映を待っています。")
		}
		e = s.startFile(ctx, p)
	default:
		return nil, problem(404, "この操作はありません。")
	}
	if e != nil {
		return nil, e
	}
	return s.present(p), nil
}

// chatProject はプロジェクトの相談へ利用者の発言を足すか失敗した応答を再試行し、受付AIに考えさせる。同じ送信IDの送り直しは今の状態を返す。
func (s *Service) chatProject(ctx context.Context, id string, p, data Object) (Object, error) {
	if q := contract.OnlyKeys(data, "id", "text", "retry"); q != nil {
		return nil, q
	}
	retry := data["retry"] == true
	requestID := fleet.Text(data["id"])
	text := strings.TrimSpace(fleet.Text(data["text"]))
	if requestID != "" && !uuidPattern.MatchString(requestID) {
		return nil, problem(400, "送信IDが不正です。")
	}
	if !retry && requestID != "" {
		for _, v := range fleet.List(p["messages"]) {
			m := fleet.ObjectOrEmpty(v)
			if m["requestId"] == requestID {
				if m["text"] != text {
					return nil, problem(409, "同じ送信IDの内容が一致しません。")
				}
				return s.present(p), nil
			}
		}
	}
	if locked(p) || p["status"] == "thinking" || s.running[id] {
		return nil, problem(409, "現在この相談へ返信できません。")
	}
	if retry {
		if p["status"] != "error" {
			return nil, problem(409, "再試行できる相談はありません。")
		}
	} else {
		if text == "" || size(text) > 8000 {
			return nil, problem(400, "相談は8000文字以内で入力してください。")
		}
		messages := append(fleet.List(p["messages"]), Object{"id": uuid(), "requestId": requestID, "role": "user", "text": text})
		if size(string(jsonBytes(messages))) > 80000 {
			return nil, problem(400, "会話が上限を超えています。")
		}
		p["messages"] = messages
	}
	if e := s.think(ctx, p); e != nil {
		return nil, e
	}
	return s.present(p), nil
}

// approveProject は利用者の同意と確認した定義の一致を確かめて作成の承認を記録し、作成できるなら作成を始める。
func (s *Service) approveProject(ctx context.Context, p, data Object, principal auth.Principal) (Object, error) {
	if q := contract.OnlyKeys(data, "fingerprint", "statement", "instructionAt"); q != nil {
		return nil, q
	}
	consent, denied := auth.UserApproval(principal, data["statement"], data["instructionAt"], s.o.Now(), "新しいプロジェクトの作成への同意")
	if denied != nil {
		return nil, denied
	}
	fingerprint := fleet.Text(data["fingerprint"])
	if fingerprint == "" || p["definition"] == nil || fingerprint != Fingerprint(fleet.ObjectOrEmpty(p["definition"])) {
		return nil, problem(409, "確認した定義と現在の定義が一致しません。")
	}
	if p["status"] != "ready" {
		if approved(p) && (p["status"] == "create_approved" || p["status"] == "creating" || p["status"] == "registered" || p["status"] == "creation_failed" || p["status"] == "creation_blocked") {
			return s.present(p), nil
		}
		return nil, problem(409, "プロジェクト定義が確定していません。")
	}
	p["approval"] = Object{"approvedAt": s.now(), "fingerprint": fingerprint, "via": "screen"}
	if consent.Statement != "" {
		approval := fleet.ObjectOrEmpty(p["approval"])
		approval["via"], approval["pc"], approval["statement"], approval["instructionAt"] = "chat", principal.Name, consent.Statement, consent.InstructionAt
	}
	p["status"] = "create_approved"
	revision(p)
	e := s.save(ctx, p)
	if e == nil && s.creatable() {
		e = s.startCreate(ctx, p)
	}
	if e != nil {
		return nil, e
	}
	return s.present(p), nil
}

func (s *Service) launch(id, kind string, run func() error) {
	s.launchTask(id, kind, s.ctx, run, func() {})
}
func (s *Service) launchTask(id, kind string, ctx context.Context, run func() error, release func()) {
	if s.running[id] {
		return
	}
	s.running[id] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer release()
		e := run()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.running, id)
		if s.ctx.Err() != nil {
			return
		}
		if e == nil {
			if kind == "create" {
				if p, err := s.load(s.ctx, id); err == nil && p["status"] == "registered" {
					contract.LogFailure("プロジェクトの起票の開始 "+id, s.startFile(ctx, p))
				}
			}
			return
		}
		p, err := s.load(context.Background(), id)
		if err != nil {
			contract.LogFailure("プロジェクトの処理 "+kind+" "+id, e)
			contract.LogFailure("プロジェクトの読取 "+id, err)
			return
		}
		message := "処理を完了できませんでした。設定と処理結果を確認してください。"
		var safe interface{ Error() string }
		if _, ok := e.(*blocked); ok {
			safe = e
		}
		if safe != nil {
			message = safe.Error()
		} else {
			// 画面の文は固定のまま、原因は記録とログへ残す。
			contract.LogFailure("プロジェクトの処理 "+kind+" "+id, e)
			p["lastFailure"] = Object{"at": s.now(), "stage": kind, "cause": contract.SafeCause(e)}
		}
		switch kind {
		case "think":
			p["status"] = "error"
			p["error"] = message
			p["queued"] = false
			p["run"] = nil
		case "create":
			c := fleet.ObjectOrEmpty(p["creation"])
			c["error"] = message
			c["stoppedAt"] = s.now()
			p["status"] = "creation_failed"
			if _, ok := e.(*blocked); ok {
				p["status"] = "creation_blocked"
				c["reason"] = message
				c["error"] = nil
			}
		case "file":
			f := fleet.ObjectOrEmpty(p["filing"])
			f["status"] = "failed"
			f["error"] = message
			f["stoppedAt"] = s.now()
		}
		revision(p)
		contract.LogFailure("プロジェクトの失敗の保存 "+id, s.save(context.Background(), p))
	}()
}
func (s *Service) think(ctx context.Context, p Object) error {
	if s.o.Answer == nil {
		return problem(503, "相談AIが設定されていません。")
	}
	p["status"] = "thinking"
	p["error"] = nil
	p["queued"] = false
	p["run"] = uuid()
	if e := s.save(ctx, p); e != nil {
		return e
	}
	id, run := fleet.Text(p["id"]), p["run"]
	snapshot := clone(p)
	s.launch(id, "think", func() error {
		prompt, _ := assets.ReadFile("prompt.md")
		schema, _ := assets.ReadFile("schema.json")
		repositories := []string{}
		if s.o.Repositories != nil {
			repositories = s.o.Repositories()
		}
		result, e := s.o.Answer(s.ctx, Object{"id": id, "provider": snapshot["consultationProvider"], "implementationProvider": snapshot["provider"], "skill": string(prompt), "schema": string(schema), "owner": s.o.Owner, "repositories": repositories, "messages": snapshot["messages"], "previousDefinition": snapshot["definition"], "handoff": snapshot["handoff"]})
		if e != nil {
			return e
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		p, e := s.load(s.ctx, id)
		if e != nil {
			return e
		}
		if p["status"] != "thinking" || p["run"] != run {
			return nil
		}
		questions := []any{}
		for _, v := range fleet.List(result["questions"]) {
			if q, ok := v.(string); ok && strings.TrimSpace(q) != "" && len(questions) < 5 {
				questions = append(questions, q)
			}
		}
		p["messages"] = append(fleet.List(p["messages"]), Object{"id": uuid(), "role": "assistant", "text": fleet.Text(result["reply"])})
		d := fleet.ObjectOrEmpty(result["project"])
		if title := strings.TrimSpace(fleet.Text(d["name"])); title != "" {
			rr := []rune(title)
			if len(rr) > 120 {
				rr = rr[:120]
			}
			p["title"] = string(rr)
		}
		var definition Object
		if result["ready"] == true && len(questions) == 0 {
			d["splitIssues"] = result["splitIssues"]
			definition, e = validate(d, s.o.Owner, repositories)
			if e != nil {
				p["status"] = "error"
				p["error"] = e.Error()
				p["questions"] = []any{}
				p["queued"] = false
				p["run"] = nil
				revision(p)
				return s.save(s.ctx, p)
			}
		}
		p["definition"] = definition
		p["fingerprint"] = nil
		p["status"] = "draft"
		if len(questions) > 0 {
			p["status"] = "waiting"
		}
		if definition != nil {
			p["fingerprint"] = Fingerprint(definition)
			p["status"] = "ready"
			if approved(p) {
				p["status"] = "create_approved"
			}
		}
		p["questions"] = questions
		p["queued"] = false
		p["run"] = nil
		revision(p)
		return s.save(s.ctx, p)
	})
	return nil
}
func (s *Service) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, e := s.all(ctx)
	if e != nil {
		return e
	}
	for _, p := range records {
		if s.running[fleet.Text(p["id"])] {
			continue
		}
		switch p["status"] {
		case "thinking":
			e = s.think(ctx, p)
		case "creating":
			e = s.startCreate(ctx, p)
		case "registered":
			if fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" {
				e = s.startFile(ctx, p)
			}
		}
		if e != nil {
			return fmt.Errorf("resume project %s: %w", fleet.Text(p["id"]), e)
		}
	}
	return nil
}
