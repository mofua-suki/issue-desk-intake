package issueapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/images"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func settingsFromBody(body string) Object {
	markers := regexp.MustCompile(`<!-- issue-desk:[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12} -->`).FindAllStringIndex(body, -1)
	if len(markers) == 0 {
		return nil
	}
	tail := body[markers[len(markers)-1][1]:]
	match := regexp.MustCompile(`^\r?\n<!-- issue-desk:settings (\{[^<>]*\}) -->`).FindStringSubmatch(tail)
	if len(match) != 2 {
		return nil
	}
	var value Object
	if json.Unmarshal([]byte(match[1]), &value) != nil || !oneOf(fleet.Text(value["provider"]), "codex", "claude", "antigravity") {
		return nil
	}
	return value
}
func (s *Service) detailFields(ctx context.Context, repo string, n int64, issue, detail Object) error {
	config, e := s.configuration()
	if e != nil {
		return e
	}
	origin := fleet.Text(fleet.ObjectOrEmpty(config["intake"])["publicUrl"])
	source := Object(nil)
	if s.o.Own(fleet.ObjectOrEmpty(issue["user"])) {
		if id := filing.DraftID(fleet.Text(issue["body"])); id != "" {
			source, e = s.read(ctx, "drafts", id)
			if e != nil {
				return e
			}
		}
	}
	submitted, e := s.submittedDraft(ctx, repo, n)
	if e != nil {
		return e
	}

	detailDraftFields(repo, n, issue, detail, source, submitted, origin)
	if value, e := s.issueOrigin(ctx, repo, fleet.Text(issue["body"]), source); e != nil {
		return e
	} else if value != nil {
		detail["origin"] = value
	}
	if e = s.detailCommentActions(ctx, repo, n, origin, detail); e != nil {
		return e
	}
	return nil
}
func (s *Service) commentImage(ctx context.Context, id, repo string, n int64, origin string) (Object, error) {
	if !uuid.MatchString(id) {
		return nil, nil
	}
	var raw, scope string
	e := s.o.AppDB.QueryRowContext(ctx, bindSQL(`SELECT json,scope FROM images WHERE id=?`), id).Scan(&raw, &scope)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if scope != fmt.Sprintf("issue:%s#%d", repo, n) {
		return nil, fail(409, "返信画像の保存範囲が一致しません。")
	}
	meta := Object{}
	if e = json.Unmarshal([]byte(raw), &meta); e != nil {
		return nil, e
	}
	query := url.Values{"project": {repo}, "number": {fmt.Sprint(n)}}
	meta["url"] = strings.TrimRight(origin, "/") + "/api/images/" + id + "?" + query.Encode()
	return meta, nil
}

var decimalNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// submittedDraft uses the saved projection for matching, falling back to the
// original text for legacy rows whose projection could not be produced.
func (s *Service) submittedDraft(ctx context.Context, repo string, n int64) (Object, error) {
	tx, err := s.o.AppDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, bindSQL("SELECT id, screen_summary, CASE WHEN screen_summary IS NULL THEN json END FROM drafts ORDER BY "+rowOrder(true)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var summary, body sql.NullString
		if err := rows.Scan(&id, &summary, &body); err != nil {
			return nil, err
		}
		raw := summary.String
		if !summary.Valid {
			raw = body.String
		}
		var draft Object
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.UseNumber()
		if decoder.Decode(&draft) != nil || draft == nil {
			continue
		}
		if draftTargetsIssue(draft, repo, n) {
			if !summary.Valid {
				return draft, nil
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if err := tx.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), id).Scan(&raw); err != nil {
				return nil, err
			}
			var full Object
			decoder := json.NewDecoder(bytes.NewBufferString(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&full); err != nil {
				return nil, err
			}
			return full, nil
		}
	}
	return nil, rows.Err()
}

// detailDraftFields は相談の記録と本文の設定から、提供元・作業種別・画像・難易度・優先度・調査の項目を詳細に足す。
func detailDraftFields(repo string, n int64, issue, detail, source, submitted Object, origin string) {
	settings := settingsFromBody(fleet.Text(issue["body"]))
	provider := fleet.Text(submitted["consultationProvider"])
	if provider == "" {
		provider = fleet.Text(settings["consultationProvider"])
	}
	if provider == "" {
		provider = "codex"
	}
	detail["consultationProvider"] = provider
	if detail["workType"] == "unknown" && detail["generation"] == nil {
		detail["workType"] = normalizedWorkType(settings["workType"])
		detail["workTypeLabel"] = workTypeLabels[fleet.Text(detail["workType"])]
	}
	photos := []any{}
	if submitted != nil {
		presented := images.Present(submitted, origin)
		for _, raw := range fleet.Items(presented["messages"]) {
			photos = append(photos, fleet.Items(fleet.ObjectOrEmpty(raw)["images"])...)
		}
	}
	detail["images"] = photos
	if source["project"] == repo {
		decision := fleet.ObjectOrEmpty(source["implementationTier"])
		bound := fleet.ObjectOrEmpty(source["issue"])
		boundRepo := fleet.Text(bound["project"])
		if boundRepo == "" {
			boundRepo = fleet.Text(source["project"])
		}
		boundNumber, _ := number(bound["number"])
		if boundNumber == n && boundRepo == repo && strings.TrimSpace(fleet.Text(decision["level"])) != "" {
			detail["implementationTier"] = Object{"level": decision["level"], "source": decision["source"], "reason": decision["reason"], "decidedAt": decision["decidedAt"]}
		}
		priority := fleet.ObjectOrEmpty(source["prioritySource"])
		if fleet.Text(priority["reason"]) != "" {
			detail["priorityRequest"] = Object{"reason": priority["reason"], "via": priority["via"]}
			if priority["pc"] != nil {
				fleet.ObjectOrEmpty(detail["priorityRequest"])["pc"] = priority["pc"]
			}
		}
		if strings.Contains(fleet.Text(issue["body"]), "<!-- issue-desk:investigation -->") {
			detail["investigation"] = true
		}
	}
}

// detailCommentActions は依頼への操作の記録を読み、画像の転送待ちの有無と、各コメントの添付画像を詳細に足す。
func (s *Service) detailCommentActions(ctx context.Context, repo string, n int64, origin string, detail Object) error {
	actions, e := s.o.AppDB.QueryContext(ctx, bindSQL("SELECT json FROM actions WHERE "+jsonText("json", "$.payload.repo")+"=? AND "+safeJSONNumber("json", "$.payload.number")+"=? ORDER BY "+rowOrder(false)), repo, n)
	if e != nil {
		return e
	}
	byComment := map[int64]Object{}
	pending := false
	for actions.Next() {
		var raw string
		if e = actions.Scan(&raw); e != nil {
			actions.Close()
			return e
		}
		value := Object{}
		if e = json.Unmarshal([]byte(raw), &value); e != nil {
			actions.Close()
			return e
		}
		id, _ := number(fleet.ObjectOrEmpty(value["comment"])["id"])
		if id > 0 {
			byComment[id] = value
		}
		payload := fleet.ObjectOrEmpty(value["payload"])
		pending = pending || oneOf(fleet.Text(payload["action"]), "reply", "retry", "resume") && value["postedComment"] != nil && value["comment"] == nil
	}
	e = actions.Err()
	actions.Close()
	if e != nil {
		return e
	}
	detail["transferPending"] = pending
	for _, raw := range fleet.Items(detail["comments"]) {
		comment := fleet.ObjectOrEmpty(raw)
		id, _ := number(comment["id"])
		images := []any{}
		for _, rawID := range fleet.Items(fleet.ObjectOrEmpty(byComment[id]["payload"])["images"]) {
			image, e := s.commentImage(ctx, fleet.Text(rawID), repo, n, origin)
			if e != nil {
				return e
			}
			if image != nil {
				images = append(images, image)
			}
		}
		comment["images"] = images
	}
	return nil
}

// detailCloseHistory は依頼を閉じた記録を詳細に足す。
func (s *Service) detailCloseHistory(ctx context.Context, repo string, n int64, detail Object) error {
	history := []any{}
	records, e := s.closeRecords(ctx, closeHistoryFilter(repo))
	if e != nil {
		return e
	}
	for _, r := range records {
		if r["project"] == repo && int64Number(r["number"]) == n && r["status"] == "closed" {
			decision := fleet.ObjectOrEmpty(r["decision"])
			history = append(history, Object{"id": r["id"], "reason": r["reason"], "pc": r["pc"], "method": decision["method"], "decisionReason": decision["reason"], "decidedAt": decision["at"], "closedAt": r["closedAt"]})
		}
	}
	if len(history) > 0 {
		detail["closeHistory"] = history
	}
	return nil
}

// draftTargetsIssue は相談の起票先が repo の Issue n かを、文字列・数値のどちらで保存した番号でも厳密に照合する。
func draftTargetsIssue(draft Object, repo string, n int64) bool {
	issue := fleet.ObjectOrEmpty(draft["issue"])
	project := fleet.Text(issue["project"])
	if project == "" {
		project = fleet.Text(draft["project"])
	}
	if project != repo {
		return false
	}
	var text string
	switch value := issue["number"].(type) {
	case string:
		text = value
	case json.Number:
		text = string(value)
	default:
		return false
	}
	text = strings.TrimSpace(text)
	if len(text) > 147456 || !decimalNumber.MatchString(text) {
		return false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(text[i+1:], 10, 32)
		if err != nil || exponent > 300000 || exponent < -300000 {
			return false
		}
	}
	value, ok := new(big.Rat).SetString(text)
	return ok && value.IsInt() && value.Num().IsInt64() && value.Num().Int64() == n
}
