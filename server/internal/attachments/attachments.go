package attachments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/images"
	"regexp"
	"strings"
	"sync"
)

const Pending = "<!-- issue-desk-images-pending -->"

type Object = map[string]any
type API interface {
	Do(context.Context, string, string, any, any) error
	UploadAttachment(context.Context, string, string, string, []byte) (map[string]any, error)
	CheckAttachmentSupport(context.Context) error
	AttachmentLink(string) string
}
type Options struct {
	AppDB   *sql.DB
	Gitea   API
	Own     func(Object) bool
	Publish func(Object)
}
type Service struct {
	o     Options
	locks sync.Map
}
type Image struct {
	ID, MessageID, SHA256, Ext, MIME string
	Size                             int64
	Bytes                            []byte
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var scope = regexp.MustCompile(`^(?:draft:[a-f0-9-]{36}|issue:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[1-9][0-9]*)$`)

func New(o Options) (*Service, error) {
	if o.AppDB == nil || o.Gitea == nil {
		return nil, errors.New("画像の保存先とGiteaが必要です")
	}
	return &Service{o: o}, nil
}
func fail(message string) error { return contract.NewProblem(409, message, "") }
func (s *Service) withLock(key string) func() {
	v, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}
func (s *Service) Selected(ctx context.Context, wanted string, ids []string, message string) ([]Image, error) {
	if !scope.MatchString(wanted) || len(ids) < 1 || len(ids) > 12 || message != "" && len(ids) > 4 || message != "" && !uuid.MatchString(message) {
		return nil, fail("画像の指定が不正です。")
	}
	out := []Image{}
	seen := map[string]bool{}
	for _, id := range ids {
		if !uuid.MatchString(id) || seen[id] {
			return nil, fail("画像の指定が重複または不正です。")
		}
		seen[id] = true
		var actual, msg, raw string
		var data []byte
		if e := s.o.AppDB.QueryRowContext(ctx, bindSQL(`SELECT scope,message,json,bytes FROM images WHERE id=?`), id).Scan(&actual, &msg, &raw, &data); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return nil, fail("画像が見つかりません。")
			}
			return nil, fmt.Errorf("画像の保存データを読み取れません: %w", e)
		}
		if actual != wanted || message != "" && msg != message {
			return nil, fail("画像の対象またはメッセージが一致しません。")
		}
		info, e := images.Inspect(data)
		if e != nil {
			return nil, e
		}
		meta := Object{}
		if json.Unmarshal([]byte(raw), &meta) != nil {
			return nil, fail("画像の保存情報を確認できません。")
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		if meta["sha256"] != hash || fleet.Int64(meta["size"]) != int64(len(data)) {
			return nil, fail("画像のハッシュが保存情報と一致しません。")
		}
		out = append(out, Image{id, msg, hash, info.Ext, info.MIME, int64(len(data)), data})
	}
	return out, nil
}
func (s *Service) Bind(ctx context.Context, selected []Image) error {
	tx, e := s.o.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, im := range selected {
		result, e := tx.ExecContext(ctx, bindSQL(`UPDATE images SET bound=1 WHERE id=? AND message=?`), im.ID, im.MessageID)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return fail("送信前に画像が削除されました。")
		}
	}
	return tx.Commit()
}
func ValidateReviews(selected []Image, reviews []any) error {
	if len(selected) != len(reviews) {
		return fail("受付AIの画像確認が不足しています。")
	}
	seen := map[string]bool{}
	for _, im := range selected {
		var review Object
		for _, raw := range reviews {
			r := fleet.ObjectOrEmpty(raw)
			if r["id"] == im.ID {
				if review != nil {
					return fail("画像確認が重複しています。")
				}
				review = r
			}
		}
		if review == nil || seen[im.ID] || review["readable"] != true {
			return fail("画像を確認済みとして扱えません。")
		}
		seen[im.ID] = true
		for _, key := range []string{"screen", "location", "change", "uncertainty"} {
			text, ok := review[key].(string)
			if !ok || contract.UTF16Length(text) > 2000 || key != "uncertainty" && strings.TrimSpace(text) == "" {
				return fail("画像の確認内容が不足しています。")
			}
		}
	}
	return nil
}
func Description(selected []Image, reviews []any, links map[string]string) string {
	if len(selected) == 0 {
		return ""
	}
	parts := []string{"\n\n## 添付画像と受付AIの確認\n"}
	for _, im := range selected {
		var r Object
		for _, v := range reviews {
			if fleet.ObjectOrEmpty(v)["id"] == im.ID {
				r = fleet.ObjectOrEmpty(v)
				break
			}
		}
		uncertainty := fleet.Text(r["uncertainty"])
		if uncertainty == "" {
			uncertainty = "なし"
		}
		text := fmt.Sprintf("\n画像ID: %s / メッセージID: %s\n対象画面: %s\n対象箇所: %s\n期待する変更: %s\n未確定事項: %s", im.ID, im.MessageID, fleet.Text(r["screen"]), fleet.Text(r["location"]), fleet.Text(r["change"]), uncertainty)
		if link := links[im.ID]; link != "" {
			text += "\n![添付画像 " + im.ID + "](" + link + ")"
		}
		parts = append(parts, text)
	}
	return strings.ReplaceAll(strings.Join(parts, "\n"), "<!--", "&lt;!--")
}
func (s *Service) Transfer(ctx context.Context, route string, selected []Image, state Object, save func() error) (map[string]string, error) {
	uploads := fleet.ObjectOrEmpty(state["uploads"])
	state["uploads"] = uploads
	links := map[string]string{}
	for _, im := range selected {
		name := fmt.Sprintf("desk-%s-%s.%s", im.ID, im.SHA256, im.Ext)
		var rows []Object
		if e := s.o.Gitea.Do(ctx, "GET", route, nil, &rows); e != nil {
			return nil, e
		}
		var asset Object
		for _, r := range rows {
			if r["name"] == name && fleet.Int64(r["size"]) == im.Size {
				if asset != nil {
					return nil, fail("Giteaの添付が重複しています。")
				}
				asset = r
			}
		}
		if asset == nil {
			if fleet.ObjectOrEmpty(uploads[im.ID])["attempted"] == true {
				return nil, fail("前回の画像転送結果を照合できません。重複転送を停止しました。")
			}
			uploads[im.ID] = Object{"attempted": true}
			if e := save(); e != nil {
				return nil, e
			}
			var e error
			asset, e = s.o.Gitea.UploadAttachment(ctx, route, name, im.MIME, im.Bytes)
			if e != nil {
				var transport *gitea.Error
				if errors.As(e, &transport) && transport.Status >= 400 && transport.Status < 500 && !transport.UnknownOutcome {
					delete(uploads, im.ID)
					_ = save()
				}
				return nil, e
			}
		}
		if asset["name"] != name || fleet.Int64(asset["size"]) != im.Size || !uuid.MatchString(fleet.Text(asset["uuid"])) {
			return nil, fail("Giteaの添付応答を照合できません。")
		}
		link := s.o.Gitea.AttachmentLink(fleet.Text(asset["uuid"]))
		if link == "" {
			return nil, fail("画像の公開URLを確認できません。")
		}
		links[im.ID] = link
		uploads[im.ID] = Object{"attempted": true, "id": asset["id"], "uuid": asset["uuid"]}
		if e := save(); e != nil {
			return nil, e
		}
	}
	return links, nil
}
func (s *Service) merge(ctx context.Context, table, id string, fields Object) error {
	tx, e := s.o.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var raw string
	if e = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM "+table+" WHERE id=?"), id).Scan(&raw); e != nil {
		return e
	}
	current := Object{}
	if e = json.Unmarshal([]byte(raw), &current); e != nil {
		return e
	}
	for k, v := range fields {
		current[k] = v
	}
	encoded, e := json.Marshal(current)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, bindSQL("UPDATE "+table+" SET json=? WHERE id=?"), string(encoded), id); e != nil {
		return e
	}
	return tx.Commit()
}

// Attach is directly compatible with filing.Options.Attach. Publication remains
// gated by Pending until all per-image receipts and final body are confirmed.
func (s *Service) Attach(ctx context.Context, draft consultations.Record, issue map[string]any) error {
	unlock := s.withLock("draft:" + fleet.Text(draft["id"]))
	defer unlock()
	id := fleet.Text(draft["imageSourceId"])
	if id == "" {
		id = fleet.Text(draft["id"])
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, raw := range fleet.Items(draft["messages"]) {
		for _, v := range fleet.Items(fleet.ObjectOrEmpty(raw)["images"]) {
			imageID := fleet.Text(fleet.ObjectOrEmpty(v)["id"])
			if !seen[imageID] {
				ids = append(ids, imageID)
				seen[imageID] = true
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	selected, e := s.Selected(ctx, "draft:"+id, ids, "")
	if e != nil {
		return e
	}
	reviews := fleet.Items(draft["imageReviews"])
	if e = ValidateReviews(selected, reviews); e != nil {
		return e
	}
	if e = s.o.Gitea.CheckAttachmentSupport(ctx); e != nil {
		return e
	}
	if e = s.Bind(ctx, selected); e != nil {
		return e
	}
	state := Object(draft)
	save := func() error {
		return s.merge(ctx, "drafts", fleet.Text(draft["id"]), Object{"uploads": state["uploads"]})
	}
	repo := fleet.Text(draft["project"])
	n := fleet.Int64(issue["number"])
	if n < 1 {
		return fail("添付先のIssue番号を確認できません。")
	}
	links, e := s.Transfer(ctx, fmt.Sprintf("/repos/%s/issues/%d/assets", repo, n), selected, state, save)
	if e != nil {
		return e
	}
	route := fmt.Sprintf("/repos/%s/issues/%d", repo, n)
	original := fleet.Text(draft["postBody"])
	if original == "" {
		original = fleet.Text(issue["body"])
	}
	if e := s.publishIssueBody(ctx, route, original, Description(selected, reviews, links)); e != nil {
		return e
	}
	return s.merge(ctx, "drafts", fleet.Text(draft["id"]), Object{"attachmentsPublished": true})
}

// The version protects edits between reading the current issue and publishing its attachments.
func (s *Service) publishIssueBody(ctx context.Context, route, original, description string) error {
	var issue Object
	if err := s.o.Gitea.Do(ctx, "GET", route, nil, &issue); err != nil {
		return err
	}
	current := fleet.Text(issue["body"])
	body := strings.TrimSuffix(strings.TrimSuffix(original, "\n"+Pending), Pending) + description
	if current == body || current == original && strings.HasSuffix(current, description) {
		return nil
	}
	if current != original || !strings.HasSuffix(original, Pending) {
		return fail("Issue本文が変更されたため添付の公開を止めました。")
	}
	if issue["content_version"] == nil {
		return fail("添付先のIssue本文の版を確認できません。")
	}
	return s.o.Gitea.Do(ctx, "PATCH", route, Object{"body": body, "content_version": issue["content_version"]}, nil)
}
