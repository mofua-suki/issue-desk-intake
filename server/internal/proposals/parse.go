package proposals

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"issue-desk-intake/server/internal/contract"
)

var proposalMarker = regexp.MustCompile(`<!-- issue-desk-proposal (\{[^<>]*\}) -->`)
var issueReference = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[1-9][0-9]{0,15}$`)
var proposalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validReference(value string) bool {
	if !issueReference.MatchString(value) {
		return false
	}
	n, e := strconv.ParseInt(value[strings.LastIndex(value, "#")+1:], 10, 64)
	return e == nil && n <= 9007199254740991
}
func validTexts(v any, max, limit int, empty bool) bool {
	a, ok := v.([]any)
	if !ok || len(a) > max || !empty && len(a) == 0 {
		return false
	}
	for _, v := range a {
		if !contract.FilledText(v, limit) {
			return false
		}
	}
	return true
}
func scope(v any, nullable bool) bool {
	if v == nil {
		return nullable
	}
	m, ok := v.(map[string]any)
	return ok && len(m) == 1 && validTexts(m["paths"], 40, 200, false)
}

// orderedJSON retains the public fingerprint's fixed field order.
func orderedJSON(m Object, keys []string) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		var value bytes.Buffer
		encoder := json.NewEncoder(&value)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(m[k])
		encoded := strings.TrimSuffix(value.String(), "\n")
		encoded = strings.ReplaceAll(strings.ReplaceAll(encoded, `\u2028`, "\u2028"), `\u2029`, "\u2029")
		b.WriteString(encoded)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// proposalKeys lists the fields of each proposal version.
func proposalKeys(version int64) ([]string, error) {
	keys := strings.Fields("version kind id source title purpose acceptance dependsOn provider")
	if version == 2 {
		keys = append(keys, "changeScope")
	} else if version == 3 {
		keys = strings.Fields("version kind id source title pr items dependsOn provider changeScope")
	} else if version != 1 {
		return nil, fail(409, "案の版に対応していません。")
	}
	return keys, nil
}

// followupItem keeps the nested item order used by the public fingerprint.
type followupItem struct {
	Problem  string `json:"problem"`
	Expected string `json:"expected"`
	Test     string `json:"test"`
}

// normalizeFollowup validates a version 3 follow-up proposal and orders its items.
func normalizeFollowup(p Object) error {
	u, e := url.Parse(str(p["pr"]))
	items, ok := p["items"].([]any)
	if p["kind"] != "followup" || e != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || !contract.FilledText(p["pr"], 500) || strings.ContainsAny(str(p["pr"]), " \t\r\n") || !ok || len(items) < 1 || len(items) > 5 || !scope(p["changeScope"], true) {
		return fail(409, "フォローアップ案の形式が不正です。")
	}
	ordered := []followupItem{}
	for _, raw := range items {
		m := obj(raw)
		if len(m) != 3 || !contract.FilledText(m["problem"], 2000) || !contract.FilledText(m["expected"], 2000) || !contract.FilledText(m["test"], 2000) {
			return fail(409, "指摘・期待動作・検証方法が不正です。")
		}
		ordered = append(ordered, followupItem{str(m["problem"]), str(m["expected"]), str(m["test"])})
	}
	p["items"] = ordered
	return nil
}

func parse(body string) (Object, string, error) {
	if strings.Count(body, "<!-- issue-desk-proposal") != 1 {
		return nil, "", fail(409, "案のマーカーを1件に確定できません。")
	}
	match := proposalMarker.FindStringSubmatch(body)
	if len(match) != 2 {
		return nil, "", fail(409, "案のマーカーの形式が不正です。")
	}
	var p Object
	if json.Unmarshal([]byte(match[1]), &p) != nil || p == nil {
		return nil, "", fail(409, "案のJSONを読めません。")
	}
	version := num(p["version"])
	keys, err := proposalKeys(version)
	if err != nil {
		return nil, "", err
	}
	if _, has := p["priority"]; has && version == 3 {
		keys = append(keys, "priority")
		priority, ok := p["priority"].(map[string]any)
		if !ok || len(priority) != 2 || priority["level"] != "high" {
			return nil, "", fail(409, "priority は level=high と reason のみを指定してください。")
		}
		reason, ok := priority["reason"].(string)
		reason = strings.TrimSpace(reason)
		if !ok || !utf8.ValidString(reason) || len([]rune(reason)) < 1 || len([]rune(reason)) > 500 {
			return nil, "", fail(409, "priority.reason は空白を除き1〜500文字で指定してください。")
		}
		p["priority"] = Object{"level": "high", "reason": reason}
	}
	if len(p) != len(keys) {
		return nil, "", fail(409, "案に不足または不明な項目があります。")
	}
	for _, k := range keys {
		if _, has := p[k]; !has {
			return nil, "", fail(409, "案に必須項目がありません。")
		}
	}
	if p["version"] != float64(version) || !proposalID.MatchString(str(p["id"])) || !validReference(str(p["source"])) || !contract.FilledText(p["title"], 200) || strings.ContainsAny(str(p["title"]), "\r\n") || !(p["kind"] == "followup" || p["kind"] == "improvements" || p["kind"] == "split") || !(p["provider"] == "codex" || p["provider"] == "claude") {
		return nil, "", fail(409, "案の識別子・対象・種類が不正です。")
	}
	deps, ok := p["dependsOn"].([]any)
	if !ok || len(deps) > 30 {
		return nil, "", fail(409, "案の依存先が不正です。")
	}
	seen := map[string]bool{}
	for _, dep := range deps {
		d := str(dep)
		if !validReference(d) || seen[d] {
			return nil, "", fail(409, "案の依存先が不正です。")
		}
		seen[d] = true
	}
	if version < 3 {
		if !contract.FilledText(p["purpose"], 8000) || !validTexts(p["acceptance"], 30, 2000, false) || version == 2 && !scope(p["changeScope"], false) {
			return nil, "", fail(409, "案の目的・受入条件・範囲が不正です。")
		}
	} else {
		if err := normalizeFollowup(p); err != nil {
			return nil, "", err
		}
	}
	sum := sha256.Sum256(orderedJSON(p, keys))
	return p, fmt.Sprintf("%x", sum), nil
}
