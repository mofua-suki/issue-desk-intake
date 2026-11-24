package filing

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

// 宣言外のファイルの変更を禁じる受入条件は、その変更で切れた参照・既存テストの期待値を直すことを除かないと、
// 実装AIが範囲外の既存テストを直せず保留する（Workerは宣言外の変更を検出して範囲を自動で広げる）。
// 除外が無い禁止の行には除外の一文を足し、範囲が確定している本文には固定の節を足す。
const prohibitionException = "ただし、この変更で切れた参照・既存テストの期待値を直すことは除く。"

const scopeHandlingSection = "## 変更範囲の扱い（受付の固定）\n" +
	"- 宣言した範囲の外でも、この変更で落ちた既存テストや切れた参照は同じ変更で直してよい（範囲は受付が自動で広げる）。" +
	"テストの削除・省略・弱体化はしない。これだけを理由に保留しない。\n" +
	"- 本文に宣言外の変更を禁じる文言があっても、上の扱いを優先する。"

var (
	prohibitionLine = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+.*(?:宣言[^。]*(?:内|のみ|だけ)で(?:行|変更|修正)|範囲外[^。]*(?:変更|修正|触)[^。]*(?:しない|禁止|ない)|[^。]*は(?:変更|修正)しない)`)
	exceptionWord   = regexp.MustCompile(`除く|除き|除外|切れた参照|期待値を直す`)
)

func allowBrokenReferenceFixes(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	fenced := false
	changed := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced || !prohibitionLine.MatchString(line) || exceptionWord.MatchString(line) {
			continue
		}
		lines[i] = strings.TrimRight(line, " ") + prohibitionException
		changed = true
	}
	return strings.Join(lines, "\n"), changed
}

func withScopeHandling(body string, scope Record) string {
	if scope["known"] != true || len(fleet.Items(scope["paths"])) == 0 || strings.Contains(body, "## 変更範囲の扱い（受付の固定）") {
		return body
	}
	return body + "\n\n" + scopeHandlingSection
}
