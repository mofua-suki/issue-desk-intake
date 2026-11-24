package filingcheck

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"unicode/utf8"

	"issue-desk-intake/server/internal/contract"
)

// checkReasonFallback は、理由が空または安全を確認できない回に保存する固定の要約。
var checkReasonFallback = map[string]string{
	"refused":            "起票前の判定が拒否しました（理由は安全を確認できないため保存していません）。",
	"evidence_requested": "起票前の判定が資料の追加を求めました（理由は安全を確認できないため保存していません）。",
	"ai_failed":          "受付AIの呼び出しに失敗しました。",
	"invalid_answer":     "起票前の判定の回答を処理できませんでした。",
	"allowed":            "起票前の判定が許可しました（理由は安全を確認できないため保存していません）。",
}

const (
	checkReasonOmitted    = "（本文省略）"
	checkReasonQuote      = "（引用省略）"
	checkReasonScanLimit  = 4000
	checkReasonWindow     = 12
	checkReasonShortLower = 4
)

var checkReasonQuoted = regexp.MustCompile(`「[^」]*」|『[^』]*』|"[^"]*"|“[^”]*”`)
var checkReasonID = regexp.MustCompile(`^[A-Za-z0-9:_.#-]{1,128}$`)

// checkReasonSourceKeys は、理由へ写ってはならない本文・会話の項目。
var checkReasonSourceKeys = map[string]bool{"body": true, "reference": true, "text": true, "title": true, "purpose": true, "shownBody": true, "shownTitle": true}

func collectCheckReasonSources(value any, out *[]string) {
	switch v := value.(type) {
	case Record:
		for key, item := range v {
			if s, ok := item.(string); ok {
				if checkReasonSourceKeys[key] && s != "" {
					*out = append(*out, s)
				}
				continue
			}
			collectCheckReasonSources(item, out)
		}
	case []any:
		for _, item := range v {
			collectCheckReasonSources(item, out)
		}
	}
}

func onlyIdentifierRunes(value []rune) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune("_./:#-", r)) {
			return false
		}
	}
	return true
}

// redactCheckReasonSources は、確認資料の本文・会話と一致する部分を理由から除く。パスや候補IDだけの一致は残す。
func redactCheckReasonSources(reason string, sources []string) string {
	joined := strings.Join(sources, "\x00")
	runes := []rune(reason)
	covered := make([]bool, len(runes))
	for i := 0; i+checkReasonWindow <= len(runes); i++ {
		window := runes[i : i+checkReasonWindow]
		if !onlyIdentifierRunes(window) && strings.Contains(joined, string(window)) {
			for j := i; j < i+checkReasonWindow; j++ {
				covered[j] = true
			}
		}
	}
	var kept strings.Builder
	for i := 0; i < len(runes); i++ {
		if !covered[i] {
			kept.WriteRune(runes[i])
			continue
		}
		kept.WriteString(checkReasonOmitted)
		for i+1 < len(runes) && covered[i+1] {
			i++
		}
	}
	out := kept.String()
	for _, source := range sources {
		for _, line := range strings.Split(source, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimSpace(strings.TrimLeft(line, "-*+ "))
			size := utf8.RuneCountInString(line)
			if size < checkReasonShortLower || size >= checkReasonWindow || onlyIdentifierRunes([]rune(line)) {
				continue
			}
			out = strings.ReplaceAll(out, line, checkReasonOmitted)
		}
	}
	return out
}

// limitCheckReason は1行・省略記号込みで500文字以内にする。
func limitCheckReason(value string) string {
	value = contract.LimitCause(value)
	if runes := []rune(value); len(runes) > 500 {
		return string(runes[:499]) + "…"
	}
	return value
}

// safeCheckReason は判定の各回に保存する理由を返す。資格情報に見える値を含むなら固定の要約にし、
// それ以外は引用と確認資料の本文・会話を除いて1行・500文字以内にする。
func safeCheckReason(result, reason string, material Record) string {
	fallback := checkReasonFallback[result]
	if strings.TrimSpace(reason) == "" || contract.MaskCredentials(reason) != reason {
		return fallback
	}
	if runes := []rune(reason); len(runes) > checkReasonScanLimit {
		reason = string(runes[:checkReasonScanLimit])
	}
	var sources []string
	collectCheckReasonSources(material, &sources)
	cleaned := limitCheckReason(redactCheckReasonSources(checkReasonQuoted.ReplaceAllString(reason, checkReasonQuote), sources))
	remainder := strings.NewReplacer(checkReasonOmitted, "", checkReasonQuote, "", " ", "", "…", "").Replace(cleaned)
	if remainder == "" || contract.MaskCredentials(cleaned) != cleaned {
		return fallback
	}
	return cleaned
}

// evidenceCheckReason は資料追加の要求を候補IDと種類だけで表す。
func evidenceCheckReason(requests []any) string {
	parts := []string{}
	for _, item := range requests {
		r := obj(item)
		id, kind := fleet.Text(r["candidateId"]), fleet.Text(r["kind"])
		if !checkReasonID.MatchString(id) || (kind != "body" && kind != "reference") {
			continue
		}
		parts = append(parts, id+"("+kind+")")
	}
	if len(parts) == 0 {
		return checkReasonFallback["evidence_requested"]
	}
	return limitCheckReason("資料の追加を要求: " + strings.Join(parts, ", "))
}

// aiFailedCheckReason は失敗の種類だけを添える。呼び出しのエラー文は保存しない。
func aiFailedCheckReason(failure Record) string {
	kind := ""
	if rows := fleet.Elements(failure["diagnostics"]); len(rows) > 0 {
		kind = fleet.Text(obj(rows[len(rows)-1])["kind"])
	}
	if kind == "" {
		return checkReasonFallback["ai_failed"]
	}
	return "受付AIの呼び出しに失敗しました（種類: " + kind + "）。"
}
