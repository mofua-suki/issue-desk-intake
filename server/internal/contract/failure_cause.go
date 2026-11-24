package contract

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:idk_|idh_)[A-Za-z0-9_-]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	// 値が複数語になる見出しは行末まで消す。下の token: 形式より先に当てて、Bearer の値だけが残らないようにする。
	regexp.MustCompile(`(?i)\b(?:proxy-)?authorization["']?\s*[:=：][^\r\n]*`),
	regexp.MustCompile(`(?i)\b(?:set-)?cookie["']?\s*[:=：][^\r\n]*`),
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(?:token|password|passwd|secret|api[_-]?key|access[_-]?key|client[_-]?secret|パスワード|トークン|合言葉)["']?\s*[:=：]\s*(?:"[^"\n]*"|'[^'\n]*'|「[^」\n]*」|\S+)`),
	regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|glpat-|sk-|xox[abprs]-)[A-Za-z0-9_-]{10,}`), regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), regexp.MustCompile(`//[^/\s:@]+:[^/\s@]+@`), regexp.MustCompile(`(?i)\b[a-f0-9]{40}\b`),
}

// MaskCredentials は文字列の中の資格情報に見える値を伏せる。
func MaskCredentials(value string) string {
	for _, pattern := range credentialPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

const causeLimit = 500

// SafeCause は記録・ログに残せる形の原因を返す。Error() に出ない包まれた原因も併記し、
// 資格情報を伏せて1行・500文字（UTF-16）以内にする。
func SafeCause(err error) string {
	if err == nil {
		return ""
	}
	return LimitCause(MaskCredentials(causeChain(err)))
}

func causeChain(err error) string {
	text := err.Error()
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(inner) {
		if part := inner.Error(); !strings.Contains(text, part) {
			text += ": " + part
		}
	}
	return text
}

// CauseText は原因の連なりを伏せる前の文字列で返す。呼び出し側で会話の本文を除いてから LimitCause へ渡す。
func CauseText(err error) string {
	if err == nil {
		return ""
	}
	return causeChain(err)
}

// LimitCause は原因を1行・500文字（UTF-16）以内へ縮める。
func LimitCause(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	var out strings.Builder
	n := 0
	for _, r := range value {
		size := 1
		if r > 65535 {
			size = 2
		}
		if n+size > causeLimit {
			out.WriteString("…")
			break
		}
		out.WriteRune(r)
		n += size
	}
	return out.String()
}

// LogFailure は利用者向けの固定文へ置き換える前の原因をログへ1行残す。応答の文は変えない。
// nil と、呼び出し元の切断・停止による取り消しは記録しない。
func LogFailure(where string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.Error("処理に失敗しました", "where", where, "cause", SafeCause(err))
}
