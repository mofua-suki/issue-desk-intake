package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sizeDate = regexp.MustCompile(`([0-9]{4})[-/年]([0-9]{1,2})[-/月]([0-9]{1,2})日?`)
var sizeQuote = regexp.MustCompile(`「([^「」\n]+)」`)
var sizeRequestTarget = regexp.MustCompile(`(?:この依頼|今回の依頼|本依頼)(?:は|を|については|について|だけは|だけを)\s*(?:分割しない|分けない|分けずに|分割せず|分割禁止|(?:1件|一件)で(?:進め|扱|実装|依頼|対応|起票|お願|処理|まとめ|行[うっ]|[。！!]|$))`)
var sizeProhibition = regexp.MustCompile(`分割しない|分割しないで|分けないで|分けずに|分割せず|分割禁止|分けない|分割しないように|1件で|一件で`)
var sizeOpposition = regexp.MustCompile(`できません|できない|賛成しない|反対|認めない|認めません|言わな|違う|違います|否定|否認|撤回|取り消|取りやめ|取り下げ|解除|無効|言わない|言っていない|言ってない|ではない|ではありません|でない|わけではない|わけじゃない|誤り|正しくない`)
var sizeAllowsSplit = regexp.MustCompile(`(?:(?:この依頼|今回の依頼|本依頼)(?:は|を|については|について)\s*|^\s*)(?:分割して|分けて)(?:(?:ください|(?:も)?(?:構いません|よい|良い|大丈夫))?[。！!]|(?:ください|(?:も)?(?:構いません|よい|良い|大丈夫))?$)`)
var sizeQuestion = regexp.MustCompile(`[?？]|ですか|ますか|でしょうか`)
var sizeWithdrawal = regexp.MustCompile(`撤回|取り消|取りやめ|取り下げ|解除|無効|否定|否認`)
var sizeRejected = regexp.MustCompile(`わけではない|訳ではない|わけじゃない|ではなく|でなく|ではない|ではありません|とは違|とは限らない|ないとは|しなくてよいわけ|禁止しない|禁止ではない|過去|以前|前回|当時|別件|別の依頼|他の依頼|別の判断|他の判断|別のissue|他のissue|日時未提示|日時不明|AI|提案|生成|架空|例|規則|ルール|説明|仮に|仮定|質問|撤回|取り消|取りやめ|解除|否定|否認|反対|無効|不要|ありません|誤り|正しくない|必要はない|必要ない|ことはない|言っていない|述べていない|求めていない|もよい|してよい|して良い|分割して|分けて`)

func validSizeDate(p string) bool {
	for _, match := range sizeDate.FindAllStringSubmatch(p, -1) {
		y, _ := strconv.Atoi(match[1])
		m, _ := strconv.Atoi(match[2])
		d, _ := strconv.Atoi(match[3])
		if y < 1 || m < 1 || m > 12 || d < 1 {
			continue
		}
		date := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		if date.Year() == y && int(date.Month()) == m && date.Day() == d {
			return true
		}
	}
	return false
}

// decisionParagraphs returns the paragraphs of the "## 利用者の判断と根拠" section outside
// fenced blocks.
func decisionParagraphs(body string) []string {
	inside, fence := false, ""
	fenceLen := 0
	var paragraph []string
	var paragraphs []string
	flush := func() {
		if len(paragraph) > 0 {
			paragraphs = append(paragraphs, strings.Join(paragraph, " "))
			paragraph = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			marker := trimmed[:1]
			count := len(trimmed) - len(strings.TrimLeft(trimmed, marker))
			if fence == "" {
				flush()
				fence, fenceLen = marker, count
			} else if marker == fence && count >= fenceLen && strings.TrimSpace(trimmed[count:]) == "" {
				fence, fenceLen = "", 0
			}
			continue
		}
		if fence != "" {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			if inside {
				flush()
				break
			}
			inside = trimmed == "## 利用者の判断と根拠"
			continue
		}
		if !inside {
			continue
		}
		if trimmed == "" {
			flush()
		} else {
			paragraph = append(paragraph, trimmed)
		}
	}
	flush()
	return paragraphs
}

// noSplitQuote checks the source section for a current, dated prohibition.
func noSplitQuote(body string, matches func(paragraph, quote string) bool) bool {
	paragraphs := decisionParagraphs(body)
	for _, p := range paragraphs {
		if sizeWithdrawal.MatchString(p) {
			return false
		}
	}
	for i, p := range paragraphs {
		if sizeRejected.MatchString(p) || sizeOpposition.MatchString(p) || !validSizeDate(p) {
			continue
		}
		if i > 0 && sizeOpposition.MatchString(paragraphs[i-1]) {
			continue
		}
		if i+1 < len(paragraphs) && sizeOpposition.MatchString(paragraphs[i+1]) {
			continue
		}
		for _, raw := range sizeQuote.FindAllStringSubmatch(p, -1) {
			quote := raw[1]
			if sizeRequestTarget.MatchString(quote) && sizeProhibition.MatchString(quote) &&
				!sizeRejected.MatchString(quote) && !sizeOpposition.MatchString(quote) && !sizeQuestion.MatchString(quote) && matches(p, quote) {
				return true
			}
		}
	}
	return false
}

func noSplitWithdrawn(messages []any) bool {
	for _, item := range messages {
		message := fleet.Object(item)
		if message["role"] != "user" {
			continue
		}
		text := fleet.Text(message["text"])
		if sizeWithdrawal.MatchString(text) || sizeOpposition.MatchString(text) ||
			sizeAllowsSplit.MatchString(text) {
			return true
		}
	}
	return false
}

// The generated body or a later source section needs a recorded statement
// with matching text and date. A quotation cannot be its own evidence.
func supportedNoSplit(body string, messages []any) bool {
	return noSplitQuote(body, func(paragraph, quote string) bool {
		for i, raw := range messages {
			message := fleet.Object(raw)
			statement := fleet.Text(message["text"])
			if message["role"] != "user" || strings.Contains(statement, "## 利用者の判断と根拠") ||
				!containsAffirmedQuote(statement, quote) || sizeRejected.MatchString(statement) || sizeOpposition.MatchString(statement) {
				continue
			}
			at, err := time.Parse(time.RFC3339, fleet.Text(message["at"]))
			if err == nil && paragraphHasDate(paragraph, at) && !noSplitWithdrawn(messages[i+1:]) {
				return true
			}
		}
		return false
	})
}

// A question about the quoted decision is different from an unrelated
// question in the next sentence.
func containsAffirmedQuote(statement, quote string) bool {
	for {
		_, after, found := strings.Cut(statement, quote)
		if !found {
			return false
		}
		suffix := after
		if end := strings.IndexAny(suffix, "。！!\n"); end >= 0 {
			suffix = suffix[:end]
		}
		if strings.HasSuffix(quote, "。") || !sizeQuestion.MatchString(suffix) {
			return true
		}
		statement = after
	}
}

func paragraphHasDate(p string, at time.Time) bool {
	// Message timestamps are stored in UTC; compare the same calendar date
	// even if an RFC3339 timestamp carries a different offset.
	at = at.UTC()
	matches := sizeDate.FindAllStringSubmatch(p, -1)
	if len(matches) != 1 {
		return false
	}
	for _, match := range matches {
		y, _ := strconv.Atoi(match[1])
		m, _ := strconv.Atoi(match[2])
		d, _ := strconv.Atoi(match[3])
		if y == at.Year() && m == int(at.Month()) && d == at.Day() {
			return true
		}
	}
	return false
}

func sizeInteger(v any) (float64, bool) {
	n, ok := numericValue(v)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 9007199254740991 && math.Trunc(n) == n
}

// sizeCandidate records numeric facts only, never the evidence quotation.
func sizeCandidate(d, answer Record, config map[string]any) (Record, string) {
	if answer["ready"] != true || fleet.Object(answer["newProject"])["needed"] == true || len(fleet.Elements(answer["splitIssues"])) != 0 {
		return nil, ""
	}
	limits := fleet.Object(fleet.Object(config["assignments"])["intakeSplit"])
	lineLimit, linesOK := sizeInteger(limits["lines"])
	fileLimit, filesOK := sizeInteger(limits["files"])
	if !linesOK || !filesOK || lineLimit == 0 || fileLimit == 0 {
		return nil, "受付の分割閾値の設定が不正です"
	}
	est := fleet.Object(answer["estimate"])
	lines, lok := sizeInteger(est["lines"])
	files, fok := sizeInteger(est["files"])
	diag := Record{"lines": nil, "files": nil, "limits": Record{"lines": lineLimit, "files": fileLimit}, "exceeded": []any{}}
	if lok {
		diag["lines"] = lines
	}
	if fok {
		diag["files"] = files
	}
	exceeded := []any{}
	if lok && lines > lineLimit {
		exceeded = append(exceeded, "lines")
	}
	if fok && files > fileLimit {
		exceeded = append(exceeded, "files")
	}
	diag["exceeded"] = exceeded
	if !lok || !fok {
		return diag, "見込みの変更行数とファイル数を両方とも0以上の整数で補完してください。"
	}
	evidence, messages := noSplitEvidence(d)
	if len(exceeded) == 0 || supportedNoSplit(evidence, messages) {
		return diag, ""
	}
	return diag, fmt.Sprintf("見込みが受付の閾値を超えています（%.0f行・%.0fファイル、閾値%.0f行・%.0fファイル）。目的・受入条件・依存を保つ分割案を返してください。", lines, files, lineLimit, fileLimit)
}

// One shared continuation for missing estimates and oversized estimates; persist
// before calling so a crash cannot result in an unbounded loop.
func (r *readBudget) checkIntakeSize(ctx context.Context, d, answer Record, config map[string]any, final bool, turnReads int) (bool, error) {
	if answer["ready"] == true && fleet.Object(answer["newProject"])["needed"] != true && len(fleet.Elements(answer["splitIssues"])) > 0 {
		return r.checkSplitSize(ctx, d, answer, config, final, turnReads)
	}
	diag, problem := sizeCandidate(d, answer, config)
	if diag == nil {
		if problem != "" {
			return false, fmt.Errorf("%s", problem)
		}
		if answer["ready"] == true && len(fleet.Elements(answer["splitIssues"])) > 0 {
			if state := fleet.Object(r.value["intakeSize"]); state != nil && state["pending"] == true {
				state["pending"] = false
				delete(state, "notice")
				if err := r.save(ctx); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	}
	state := fleet.Object(r.value["intakeSize"])
	if state == nil {
		state = Record{}
	}
	diag["requests"] = fleet.Number(state["requests"])
	diag["continuations"] = fleet.Number(state["continuations"])
	if previous := fleet.Object(state["lastRequest"]); previous != nil {
		diag["lastRequest"] = previous
	}
	diag["pending"] = false
	if problem != "" && diag["continuations"] == float64(0) && !final && turnReads < 6 && !r.ending() && r.budget.check() == nil {
		diag["continuations"] = 1
		if diag["lines"] != nil && diag["files"] != nil && len(fleet.Elements(diag["exceeded"])) > 0 {
			diag["requests"] = fleet.Number(diag["requests"]) + 1
			diag["lastRequest"] = Record{"lines": diag["lines"], "files": diag["files"], "limits": diag["limits"], "exceeded": diag["exceeded"]}
		}
		diag["pending"] = true
		diag["notice"] = problem
		r.value["rounds"] = fleet.Number(r.value["rounds"]) + 1
		r.value["answerPending"] = false
		r.value["intakeSize"] = diag
		if err := r.saveContinuation(ctx, answer); err != nil {
			return false, err
		}
		return true, nil
	}
	r.value["intakeSize"] = diag
	if err := r.save(ctx); err != nil {
		return false, err
	}
	if problem != "" {
		answer["ready"] = false
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n" + problem + " 自動の追加確認を終了し、下書きを残しました。")
	}
	return false, nil
}

// Split candidates receive an advisory review, never a size-based filing hold.
func (r *readBudget) checkSplitSize(ctx context.Context, d, answer Record, config map[string]any, final bool, turnReads int) (bool, error) {
	state := fleet.Object(r.value["intakeSize"])
	if state == nil {
		state = Record{}
	}
	limits := fleet.Object(fleet.Object(config["assignments"])["intakeSplit"])
	lineLimit, linesOK := sizeInteger(limits["lines"])
	fileLimit, filesOK := sizeInteger(limits["files"])
	splits := fleet.Elements(answer["splitIssues"])
	locations := []string{}
	evidence, messages := noSplitEvidence(d)
	if len(splits) > 1 && linesOK && filesOK && lineLimit > 0 && fileLimit > 0 && !supportedNoSplit(evidence, messages) {
		for i, raw := range splits {
			child := fleet.Object(raw)
			est := fleet.Object(child["estimate"])
			lines, lok := sizeInteger(est["lines"])
			files, fok := sizeInteger(est["files"])
			if len(fleet.Elements(child["acceptance"])) >= 2 && (lok && lines > lineLimit || fok && files > fileLimit) {
				locations = append(locations, fmt.Sprintf("分割案%d", i+1))
			}
		}
	}
	if len(locations) > 0 && fleet.Number(state["continuations"]) == 0 && !final && turnReads < 6 && !r.ending() && r.budget.check() == nil {
		state["continuations"] = 1
		state["pending"] = true
		state["notice"] = fmt.Sprintf("%s の受入条件が成果物や同じ検証で結び付いているか、独立してマージできる挙動が残っていないか見直してください。依存・利用者の判断・受入条件を落とさず、必要なら分割案を修正してください。見直し前の候補（データ）: %s", strings.Join(locations, "・"), string(marshal(answer)))
		r.value["rounds"] = fleet.Number(r.value["rounds"]) + 1
		r.value["answerPending"] = false
		r.value["intakeSize"] = state
		if err := r.saveContinuation(ctx, answer); err != nil {
			return false, err
		}
		return true, nil
	}
	if state["pending"] == true {
		state["pending"] = false
		delete(state, "notice")
		r.value["intakeSize"] = state
		return false, r.save(ctx)
	}
	return false, nil
}
