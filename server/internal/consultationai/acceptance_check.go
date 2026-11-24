package consultationai

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
	"regexp"
	"strings"
)

var acceptanceBullet = regexp.MustCompile(`^\s*(?:[-+*]|[0-9]+[.)])\s+`)
var acceptanceHeading = regexp.MustCompile(`^\s*(#{1,6})\s+(.+?)\s*#*\s*$`)
var acceptanceMethod = regexp.MustCompile("(?i)(`[A-Za-z0-9_./-]+(?:\\s+[^`]*)?`|\\b(?:GET|POST|PUT|PATCH|DELETE)\\s+/\\S+|\\bTest[A-Za-z0-9_]+|(?:^|\\s)[A-Za-z0-9_./-]+\\s+(?:[A-Za-z0-9_./-]+|--?[A-Za-z0-9_-]+))")
var acceptanceExecutable = regexp.MustCompile(`(?:^|\s)(?:\.{1,2}/|/)[A-Za-z0-9_./-]+(?:\s|$)`)
var acceptanceStandaloneCommand = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
var acceptancePlaceholder = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_./-])(?:tbd|todo|tba|n/?a)(?:$|[^A-Za-z0-9_./-])|未定|未確認|検討中|未記入|不明`)
var acceptanceExpectedPlaceholder = regexp.MustCompile(`(?i)(?:^|[、。\n])\s*(?:[^、。\n]*[はが:：]\s*)?(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)\s*(?:です|である)?\s*(?:[。、.,（(]|$)`)
var acceptanceAlternativeCondition = regexp.MustCompile(`^\s*(?:または|もしくは|あるいは)([^、。\n]*?)(?:のとき|の場合)`)
var acceptancePlaceholderOnly = regexp.MustCompile(`(?i)^\s*(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)\s*$`)
var acceptanceDisplayedPlaceholder = regexp.MustCompile(`(?i)(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)(?:[」』"'` + "`" + `]?と)(?:表示|出力|記録|保存)`)
var acceptanceQuotedPlaceholder = regexp.MustCompile(`(?i)(?:「(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)」|『(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)』|"(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)"|'(?:tbd|todo|tba|n/?a|未定|未確認|検討中|未記入|不明)')`)
var acceptanceDisplayVerb = regexp.MustCompile(`表示|出力|記録|保存`)
var acceptanceExitCode = regexp.MustCompile(`(?i)(?:終了コード|終了ステータス|exit\s+(?:code|status))`)
var acceptanceExitValue = regexp.MustCompile(`(?i)^(?:\s|は|が|を|で|:|：|=|と|、|,|is\b|成功を示す)*(?:[0-9０-９]+|ゼロ|非ゼロ|non[- ]?zero)`)
var acceptanceExitQuestion = regexp.MustCompile(`(?i)^\s*(?:か(?:を|どうか|否か|確認|検証|調べ|調査|判定)|なのか|\?|？)`)
var acceptanceNumericQuestion = regexp.MustCompile(`[0-9０-９]+\s*(?:件|個|回|秒|分|時間|日|%|％|人|台|行|項目|レコード|バイト)?\s*(?:であるか|なのか|か(?:を|どうか|否か|確認|検証|調べ|調査|判定)|[?？])`)
var acceptanceCountOnly = regexp.MustCompile(`^(?:件数|個数|回数|数量|数)(?:(?:は|が)(?:正しい|正常|適切|期待通り)(?:である)?(?:こと)?|(?:(?:を|は|が)(?:正しい|正常|適切|期待通り)(?:こと|であること)?を|(?:を|は|が)?)(?:確認|検証|調査|計測|測定|取得)(?:する|します)?(?:こと)?)[。.]?$`)
var acceptanceMissingMethod = regexp.MustCompile(`^(?:未設定|なし)(?:$|[\s:：、。.,;；(（])`)
var acceptanceScreen = regexp.MustCompile(`画面\s*[:：]\s*\S+.*(開く|押す|選ぶ|選択|入力|操作|クリック|タップ|遷移|送信|表示する|切り替|検索|スクロール)`)
var acceptanceVagueResult = regexp.MustCompile(`^(?:(?:画面|結果|内容|情報|データ|メッセージ|値|状態)(?:に|が|を|は))?(?:(?:正常|正しく|適切|期待通り|問題なく)(?:に|で|な|の)?)?(?:表示|出力|状態|件数|実行|確認|検証|テスト|成功|終了|完了|動作)(?:する|される)?(?:こと)?(?:を確認する)?[。.]?$`)
var acceptanceVagueCompound = regexp.MustCompile(`^(?:(?:正常に|正しく|適切に|期待通りに|問題なく)?(?:動作|実行|表示|処理)(?:する|して|し|される|されて)|(?:画面|結果|内容|情報|データ|メッセージ|値|状態)(?:が|は|も)(?:正しい|正常|適切|期待通り)(?:である|になる)?|[、。\s])+(?:こと)?(?:を確認する)?[。.]?$`)
var acceptanceVagueStateDisplay = regexp.MustCompile(`^(?:画面|結果|内容|情報|データ|メッセージ|値|状態)(?:が|は|を|も)(?:正しく|正常に|適切に|期待通りに|問題なく)(?:表示|出力|更新|保存|反映)(?:する|される)(?:こと)?(?:を確認する)?[。.]?$`)

// acceptanceItemProblem names what one acceptance item lacks: the separator, a concrete
// check method or a concrete expected result.
func acceptanceItemProblem(location, item string) string {
	parts := strings.SplitN(item, "→", 2)
	method, expected := "", ""
	if p := strings.SplitN(parts[0], "確認:", 2); len(p) == 2 {
		method = strings.TrimSpace(p[1])
	} else if p := strings.SplitN(parts[0], "確認：", 2); len(p) == 2 {
		method = strings.TrimSpace(p[1])
	}
	if len(parts) == 2 {
		if p := strings.SplitN(parts[1], "期待:", 2); len(p) == 2 {
			expected = strings.TrimSpace(p[1])
		} else if p := strings.SplitN(parts[1], "期待：", 2); len(p) == 2 {
			expected = strings.TrimSpace(p[1])
		}
	} else if p := strings.SplitN(parts[0], "期待:", 2); len(p) == 2 {
		expected = strings.TrimSpace(p[1])
	} else if p := strings.SplitN(parts[0], "期待：", 2); len(p) == 2 {
		expected = strings.TrimSpace(p[1])
	}

	missing := []string{}
	if len(parts) != 2 && method != "" && expected != "" {
		missing = append(missing, "区切り「→」")
	}
	if acceptancePlaceholder.MatchString(method) || acceptanceMissingMethod.MatchString(strings.Trim(strings.TrimSpace(method), "`")) || (!acceptanceMethod.MatchString(method) && !acceptanceExecutable.MatchString(method) && !acceptanceStandaloneCommand.MatchString(method) && !acceptanceScreen.MatchString(method)) {
		missing = append(missing, "具体的な確認手段")
	}
	placeholderCheck := removeDisplayedQuotedPlaceholders(expected)
	placeholderCheck = acceptanceDisplayedPlaceholder.ReplaceAllString(placeholderCheck, "")
	if expected == "" || hasExpectedPlaceholder(placeholderCheck) || missingExitValue(expected) || acceptanceNumericQuestion.MatchString(expected) || acceptanceCountOnly.MatchString(strings.TrimSpace(expected)) || acceptanceVagueResult.MatchString(strings.TrimSpace(expected)) || acceptanceVagueCompound.MatchString(strings.TrimSpace(expected)) || acceptanceVagueStateDisplay.MatchString(strings.TrimSpace(expected)) {
		missing = append(missing, "具体的な期待結果")
	}
	if len(missing) > 0 {
		return location + "：" + strings.Join(missing, "・") + "が不足"
	}
	return ""
}

func acceptanceProblems(answer Record) []string {
	if answer["ready"] != true {
		return nil
	}
	problems := []string{}
	check := func(location, item string) {
		if problem := acceptanceItemProblem(location, item); problem != "" {
			problems = append(problems, problem+"（条件の内容：「"+acceptanceExcerpt(item)+"」）")
		}
	}
	// A plan condition may contain line breaks. It fails when any of three readings fails: the whole
	// condition on one line, the method on one line with only the first line of the expected result
	// (so a vague result cannot pass by gaining a line, while a multi-line method stays valid), and
	// the original text with its line breaks and without list markers, plus each continuation line of
	// the expected result read alone for a placeholder (so a placeholder on a continuation line still fails).
	checkPlanCondition := func(location, condition string) {
		joined := strings.Join(strings.Fields(condition), " ")
		readings := []string{joined}
		if i := strings.Index(condition, "→"); i >= 0 {
			method := strings.Join(strings.Fields(condition[:i]), " ")
			lines := strings.Split(strings.TrimSpace(condition[i+len("→"):]), "\n")
			readings = append(readings, method+" → "+strings.TrimSpace(lines[0]))
			// Each continuation line of the expected result, written as a list item or not, is read
			// for a placeholder on its own.
			for _, line := range lines[1:] {
				line = strings.TrimSpace(acceptanceBullet.ReplaceAllString(line, ""))
				shown := acceptanceDisplayedPlaceholder.ReplaceAllString(removeDisplayedQuotedPlaceholders(line), "")
				if line != "" && hasExpectedPlaceholder(shown) {
					readings = append(readings, method+" → 期待: "+line)
				}
			}
		}
		// The original text keeps its line breaks, with list markers removed from each line.
		original := strings.Split(condition, "\n")
		for i, line := range original {
			original[i] = acceptanceBullet.ReplaceAllString(line, "")
		}
		readings = append(readings, strings.Join(original, "\n"))
		for _, reading := range readings {
			if acceptanceItemProblem(location, reading) != "" {
				check(location, reading)
				return
			}
		}
	}
	// With an execution plan the checked conditions are the plan's, not the body the requester
	// sees, so each condition is checked with its requirement directly instead of re-reading the
	// rebuilt body, whose lines a condition containing line breaks would split or end.
	if plan, err := workplan.Parse(answer["executionPlan"]); err == nil && plan != nil {
		checkPlan := func(prefix string, p *workplan.Plan) {
			for _, r := range p.Requirements {
				if r.EvidenceKind == "fixed_test" {
					checkPlanCondition(prefix+"実行計画の要件 "+r.ID, executableCondition(r))
				}
			}
		}
		checkPlan("", plan)
		for i := range fleet.Elements(answer["splitIssues"]) {
			checkPlan(fmt.Sprintf("分割案%dの", i+1), plan.ForChild(i+1))
		}
		return problems
	}
	inSection, fence := false, byte(0)
	fenceLen, start := 0, 0
	item := ""
	hasItem := false
	flush := func() {
		if hasItem {
			check(fmt.Sprintf("本文 %d行", start), item)
			item = ""
			hasItem = false
		}
	}
	for i, line := range strings.Split(fleet.Text(answer["body"]), "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			c := trimmed[0]
			n := 0
			for n < len(trimmed) && trimmed[n] == c {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					fence = c
					fenceLen = n
				} else if c == fence && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
					fence = 0
				}
				continue
			}
		}
		if fence != 0 {
			continue
		}
		if h := acceptanceHeading.FindStringSubmatch(line); h != nil {
			if len(h[1]) <= 2 {
				flush()
				inSection = len(h[1]) == 2 && strings.TrimSpace(h[2]) == "受入条件"
			}
			continue
		}
		if !inSection {
			continue
		}
		if acceptanceBullet.MatchString(line) {
			flush()
			start = i + 1
			hasItem = true
			item = acceptanceBullet.ReplaceAllString(line, "")
		} else if hasItem && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			item += "\n" + trimmed
		} else {
			flush()
		}
	}
	flush()
	for i, raw := range fleet.Elements(answer["splitIssues"]) {
		for j, condition := range fleet.Elements(fleet.Object(raw)["acceptance"]) {
			check(fmt.Sprintf("分割案%dの受入条件%d", i+1, j+1), fleet.Text(condition))
		}
	}
	return problems
}

func hasExpectedPlaceholder(s string) bool {
	for _, p := range acceptanceExpectedPlaceholder.FindAllStringIndex(s, -1) {
		if strings.HasSuffix(s[p[0]:p[1]], "、") {
			alternative := acceptanceAlternativeCondition.FindStringSubmatch(s[p[1]:])
			if alternative != nil && strings.TrimSpace(alternative[1]) != "" && !acceptancePlaceholderOnly.MatchString(alternative[1]) {
				continue
			}
		}
		return true
	}
	return false
}

func removeDisplayedQuotedPlaceholders(s string) string {
	indices := acceptanceQuotedPlaceholder.FindAllStringIndex(s, -1)
	for i := len(indices) - 1; i >= 0; i-- {
		p := indices[i]
		tail := s[p[1]:]
		if end := strings.IndexAny(tail, "、。\n"); end >= 0 {
			tail = tail[:end]
		}
		if acceptanceDisplayVerb.MatchString(tail) {
			s = s[:p[0]] + s[p[1]:]
		}
	}
	return s
}

func missingExitValue(s string) bool {
	for _, p := range acceptanceExitCode.FindAllStringIndex(s, -1) {
		tail := s[p[1]:]
		value := acceptanceExitValue.FindStringIndex(tail)
		if value == nil || acceptanceExitQuestion.MatchString(tail[value[1]:]) {
			return true
		}
	}
	return false
}

// acceptanceExcerpt clips the content of a failed condition for the question.
func acceptanceExcerpt(item string) string {
	text := strings.Join(strings.Fields(item), " ")
	if runes := []rune(text); len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return text
}

const acceptanceQuestionListed = 5

// acceptanceQuestion names the first failed conditions and what each lacks, so the requester can
// fix the right ones instead of repeating an answer the check rejects.
func acceptanceQuestion(problems []string) string {
	lines := []string{"受入条件の確認手段または期待結果を確定できませんでした。次の条件を、確認の方法（コマンド・画面）→ 期待する結果 の形で書き直してください。"}
	for i, problem := range problems {
		if i == acceptanceQuestionListed {
			lines = append(lines, fmt.Sprintf("ほか%d件", len(problems)-i))
			break
		}
		lines = append(lines, "- "+problem)
	}
	return strings.Join(lines, "\n")
}

// acceptanceReason is the short reason kept beside the question: only the locations and what each
// lacks, without the condition contents the question already lists.
func acceptanceReason(problems []string) string {
	parts := []string{}
	for i, problem := range problems {
		if i == acceptanceQuestionListed {
			parts = append(parts, fmt.Sprintf("ほか%d件", len(problems)-i))
			break
		}
		parts = append(parts, strings.SplitN(problem, "（条件の内容：", 2)[0])
	}
	return fmt.Sprintf("受入条件%d件が不足：", len(problems)) + strings.Join(parts, "、")
}
