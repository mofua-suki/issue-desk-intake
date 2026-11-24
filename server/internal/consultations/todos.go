package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

var recordID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Options struct {
	PublishDraft       func(Record)
	SubmissionQueued   func(Record)
	FamilyJobs         FamilyJobs
	FamilyIssues       FamilyIssues
	Submit             func(context.Context, string, Record, auth.Principal) (Record, error)
	Active             func(string) bool
	Cancel             func(string)
	StopExecution      func(string, stopChange) (Record, error)
	Records            *Records
	Auth               *auth.Authenticator
	PublicURL          string
	Now                func() time.Time
	Project            func(string) error
	RepositoryArchived func(context.Context, string) (bool, error)
	Providers          func() []string
	InitialTier        func(string, string, time.Time) (Record, error)
	ChangeTier         func(Record, string, time.Time) (Record, error)
	Start              func(context.Context, string, Record, auth.Principal) (Record, error)
}

type Service struct {
	options          Options
	submissionEngine *Engine
	engineMu         sync.RWMutex
	screenInvalidLog screenInvalidLogState
	timingNow        func() time.Time
}

type engineFunctions struct {
	Start         func(context.Context, string, Record, auth.Principal) (Record, error)
	Active        func(string) bool
	Cancel        func(string)
	StopExecution func(string, stopChange) (Record, error)
}

// engineSnapshot copies the shared options while holding the same lock as BindEngine.
// Direct options assignments are supported only before concurrent use.
func (s *Service) engineSnapshot() engineFunctions {
	s.engineMu.RLock()
	defer s.engineMu.RUnlock()
	return engineFunctions{s.options.Start, s.options.Active, s.options.Cancel, s.options.StopExecution}
}

func New(options Options) (*Service, error) {
	if options.Records == nil || options.Auth == nil || options.Project == nil {
		return nil, refuse(500, "相談の保存先・認証・プロジェクト設定が必要です。")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.InitialTier == nil {
		options.InitialTier = func(provider, requested string, at time.Time) (Record, error) {
			return Record{"level": "fixed", "source": "fixed", "reason": "難易度の設定が無いため固定値で相談します", "decidedAt": timestamp(at), "history": []any{}}, nil
		}
	}
	if options.ChangeTier == nil {
		options.ChangeTier = func(d Record, requested string, at time.Time) (Record, error) { return record(d["tier"]), nil }
	}
	if options.Providers == nil {
		options.Providers = func() []string { return []string{"codex", "claude"} }
	}
	return &Service{options: options}, nil
}

func owner(p auth.Principal) string {
	if p.Kind == auth.PC {
		return "token:" + p.TokenID
	}
	return ""
}
func (s *Service) checkProject(project string) error {
	if project == "" {
		return refuse(400, "許可されたプロジェクトを選んでください。")
	}
	return s.options.Project(project)
}
func (s *Service) provider(v string) (string, error) {
	if v == "" {
		v = "codex"
	}
	for _, p := range s.options.Providers() {
		if v == p {
			return v, nil
		}
	}
	return "", refuse(400, "この実行先は使えません。")
}
func consultationProvider(v string) (string, error) {
	if v == "" {
		return "codex", nil
	}
	if v != "codex" && v != "claude" {
		return "", refuse(400, "相談元はcodexまたはclaudeを指定してください。")
	}
	return v, nil
}

func (s *Service) CreateTodo(ctx context.Context, data Record, p auth.Principal) (Record, error) {
	id := text(data["id"])
	if !recordID.MatchString(id) {
		return nil, refuse(400, "TODO IDが不正です。")
	}
	project := text(data["project"])
	if err := s.checkProject(project); err != nil {
		return nil, err
	}
	provider, err := s.provider(text(data["provider"]))
	if err != nil {
		return nil, err
	}
	cp, err := consultationProvider(text(data["consultationProvider"]))
	if err != nil {
		return nil, err
	}
	return s.options.Records.Change(ctx, "todos", id, func(tx *sql.Tx, current Record) (Record, error) {
		if current != nil {
			if text(current["project"]) != project || owner(p) != "" && text(current["owner"]) != owner(p) {
				return nil, refuse(409, "TODO IDは別のTODOで使用済みです。")
			}
			existingCP := text(current["consultationProvider"])
			if existingCP == "" {
				existingCP = "codex"
			}
			if owner(p) != "" && (text(current["provider"]) != provider || existingCP != cp) {
				return nil, refuse(409, "TODO IDは別の実行先で使用済みです。")
			}
			return current, nil
		}
		now := timestamp(s.options.Now())
		r := Record{"id": id, "project": project, "provider": provider, "body": "", "images": []any{}, "revision": 0, "createdAt": now, "updatedAt": now}
		if cp != "codex" {
			r["consultationProvider"] = cp
		}
		if owner(p) != "" {
			r["owner"] = owner(p)
		}
		return r, nil
	})
}

func (s *Service) todoAccess(current Record, p auth.Principal) error {
	if current == nil {
		return refuse(404, "TODOが見つかりません。")
	}
	if p.Kind == auth.PC && !p.Delegated && text(current["owner"]) != owner(p) {
		return refuse(404, "TODOが見つかりません。")
	}
	return s.checkProject(text(current["project"]))
}

func (s *Service) SaveTodo(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	return s.options.Records.Change(ctx, "todos", id, func(tx *sql.Tx, current Record) (Record, error) {
		if err := s.todoAccess(current, p); err != nil {
			return nil, err
		}
		requestID := text(data["id"])
		if _, present := data["id"]; present && !recordID.MatchString(requestID) {
			return nil, refuse(400, "送信IDが不正です。")
		}
		body, ok := data["text"].(string)
		if !ok || contract.UTF16Length(strings.TrimSpace(body)) < 1 || contract.UTF16Length(body) > 8000 {
			return nil, refuse(400, "TODOの内容を1〜8000文字で入力してください。")
		}
		now := s.options.Now()
		fields, err := todoFields(current, data, now)
		if err != nil {
			return nil, err
		}
		images, err := s.todoImages(ctx, tx, id, data, p, current)
		if err != nil {
			return nil, err
		}
		cp := text(data["consultationProvider"])
		if p.Kind == auth.PC || cp == "" {
			cp = text(current["consultationProvider"])
		}
		cp, err = consultationProvider(cp)
		if err != nil {
			return nil, err
		}
		oldCP := text(current["consultationProvider"])
		if oldCP == "" {
			oldCP = "codex"
		}
		if requestID != "" && text(current["requestId"]) == requestID {
			if strings.TrimSpace(body) != text(current["body"]) || cp != oldCP || !sameImageIDs(images, current["images"]) {
				return nil, refuse(409, "送信IDは保存済みの内容で使用済みです。")
			}
			return current, nil
		}
		revision, valid := integer(data["revision"])
		previous, _ := integer(current["revision"])
		if !valid || revision != previous {
			return nil, refuse(409, "TODOは別の画面から更新されています。")
		}
		provider := text(current["provider"])
		if p.Kind != auth.PC && text(data["provider"]) != "" {
			provider = text(data["provider"])
		}
		provider, err = s.provider(provider)
		if err != nil {
			return nil, err
		}
		next := clone(current)
		for k, v := range fields {
			next[k] = v
		}
		next["body"], next["provider"], next["consultationProvider"], next["images"], next["revision"], next["updatedAt"] = strings.TrimSpace(body), provider, cp, images, previous+1, timestamp(now)
		if requestID != "" {
			next["requestId"] = requestID
		} else {
			delete(next, "requestId")
		}
		if p.Kind == auth.PC {
			next["savedBy"] = p.Name
		} else {
			delete(next, "savedBy")
		}
		keep := map[string]bool{}
		for _, i := range images {
			keep[text(i["id"])] = true
			if _, err := tx.ExecContext(ctx, bindSQL("UPDATE images SET bound=1 WHERE id=? AND scope=?"), text(i["id"]), "todo:"+id); err != nil {
				return nil, err
			}
		}
		for _, i := range imageRecords(current["images"]) {
			if !keep[text(i["id"])] {
				if _, err := tx.ExecContext(ctx, "DELETE FROM images WHERE id=? AND scope=?", text(i["id"]), "todo:"+id); err != nil {
					return nil, err
				}
			}
		}
		return next, nil
	})
}

func imageRecords(value any) []Record {
	raw, _ := json.Marshal(value)
	var rows []Record
	json.Unmarshal(raw, &rows)
	return rows
}
func sameImageIDs(a []Record, b any) bool {
	other := imageRecords(b)
	if len(a) != len(other) {
		return false
	}
	for i := range a {
		if text(a[i]["id"]) != text(other[i]["id"]) {
			return false
		}
	}
	return true
}

func (s *Service) todoImages(ctx context.Context, tx *sql.Tx, id string, data Record, p auth.Principal, current Record) ([]Record, error) {
	var ids []string
	_, provided := data["images"]
	if p.Kind == auth.PC || p.Kind == auth.CLI && !provided {
		for _, i := range imageRecords(current["images"]) {
			ids = append(ids, text(i["id"]))
		}
	} else if provided {
		raw, _ := json.Marshal(data["images"])
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, refuse(400, "画像の選択が不正です。")
		}
	}
	if len(ids) > 4 {
		return nil, refuse(400, "画像の選択が不正です。")
	}
	seen := map[string]bool{}
	result := make([]Record, 0, len(ids))
	for _, image := range ids {
		if seen[image] || !recordID.MatchString(image) {
			return nil, refuse(400, "画像の選択が不正です。")
		}
		seen[image] = true
		var raw string
		err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM images WHERE id=? AND scope=?"), image, "todo:"+id).Scan(&raw)
		if err == sql.ErrNoRows {
			if p.Kind == auth.PC || p.Kind == auth.CLI && !provided {
				continue
			}
			return nil, refuse(403, "別の依頼・メッセージの画像は使用できません。")
		}
		if err != nil {
			return nil, err
		}
		meta, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, meta)
	}
	return result, nil
}

// BindEngine is called during startup before routes or recovery are enabled.
func (s *Service) BindEngine(engine *Engine) {
	s.engineMu.Lock()
	defer s.engineMu.Unlock()
	s.submissionEngine = engine
	s.options.Start = engine.Start
	s.options.Active = engine.Busy
	s.options.Cancel = engine.Cancel
	s.options.StopExecution = engine.stopExecution
}
