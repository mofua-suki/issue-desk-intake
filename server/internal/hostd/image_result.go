package hostd

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Text fields accept at most 200 UTF-8 bytes; redaction happens before the
// stored value is capped at the same limit. Any future display must impose
// its own display limit, independently of the storage limit.
func imageText(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || !utf8.ValidString(s) || strings.ContainsRune(s, 0) || len(s) > 200 {
		return "", false
	}
	s = usage.Redact(s)
	for len(s) > 200 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s, true
}

var imageTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)

func (s *Service) ImageResult(ctx context.Context, host string, body map[string]any) (any, error) {
	f, err := parseImageResult(body)
	if err != nil {
		return nil, err
	}
	stage, reason, detail, fromTag, startedAt, checks := f.stage, f.reason, f.detail, f.fromTag, f.startedAt, f.checks
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES($1,$2,$3,$4,$5) ON CONFLICT(host,tag,flavor) DO UPDATE SET status=CASE WHEN host_image_results.status='failed' THEN 'failed' ELSE excluded.status END,at=now()`, host, body["tag"], body["flavor"], body["digest"], body["status"])
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO host_image_history(host,tag,flavor,digest,status,stage,reason,detail,self_check_failed,from_tag,started_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11)`, host, body["tag"], body["flavor"], body["digest"], body["status"], stage, reason, detail, checks, fromTag, startedAt)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"host": host, "status": body["status"]}, nil
}

// imageResultFields はイメージ結果の任意の項目を、保存する形にしたもの。
type imageResultFields struct {
	stage, reason, detail, fromTag, startedAt, checks any
}

// parseImageResult はイメージ結果の項目の形を検査し、保存する形にする。
func parseImageResult(body map[string]any) (imageResultFields, error) {
	invalid := func() (imageResultFields, error) {
		return imageResultFields{}, problem(400, "イメージ結果の形が違います。")
	}
	if !only(body, "tag", "flavor", "digest", "status", "stage", "reason", "detail", "selfCheckFailed", "fromTag", "startedAt") || !tagPattern.MatchString(fleet.Text(body["tag"])) || !digestPattern.MatchString(fleet.Text(body["digest"])) || !one(body["flavor"], "", "-python") || !one(body["status"], "success", "failed") {
		return invalid()
	}
	var stage, reason, detail, fromTag, startedAt, checks any
	if v, ok := body["stage"]; ok {
		if !one(v, "recover", "docker", "client", "swap-finish", "acquire", "swap-start", "apps", "complete") {
			return invalid()
		}
		stage = v
	}
	for key, dst := range map[string]*any{"reason": &reason, "detail": &detail} {
		if v, ok := body[key]; ok {
			value, valid := imageText(v)
			if !valid {
				return invalid()
			}
			*dst = value
		}
	}
	if v, ok := body["fromTag"]; ok {
		str, valid := v.(string)
		if !valid || !tagPattern.MatchString(str) {
			return invalid()
		}
		fromTag = str
	}
	if v, ok := body["startedAt"]; ok {
		str, valid := v.(string)
		if !valid || !imageTimestamp.MatchString(str) {
			return invalid()
		}
		parsed, err := time.Parse(time.RFC3339Nano, str)
		if err != nil || parsed.Year() == 0 || parsed.UTC().Year() < 1 || parsed.UTC().Year() > 9999 {
			return invalid()
		}
		startedAt = parsed
	}
	if v, ok := body["selfCheckFailed"]; ok {
		list, valid := v.([]any)
		if !valid || len(list) > 40 {
			return invalid()
		}
		result := make([]string, 0, len(list))
		for _, item := range list {
			value, valid := imageText(item)
			if !valid {
				return invalid()
			}
			result = append(result, value)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return imageResultFields{}, err
		}
		checks = string(raw)
	}
	return imageResultFields{stage: stage, reason: reason, detail: detail, fromTag: fromTag, startedAt: startedAt, checks: checks}, nil
}
