package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

// The same patterns as filing.acceptanceCount and its section heading, so a
// body that filing would count shows the same number here.
var heavyAcceptanceStart = regexp.MustCompile(`^##\s*受入条件`)
var heavyAcceptanceEnd = regexp.MustCompile(`^#{1,2}\s`)
var heavyAcceptanceBullet = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+\S`)

// noSplitEvidence returns the saved body and user messages that may establish
// a user's quoted no-split decision. The intake size check uses the same source.
func noSplitEvidence(d Record) (string, []any) {
	evidence := fleet.Text(d["body"])
	messages := fleet.Elements(d["messages"])
	// Each input-overflow split prepends a synthesized message with the user role.
	if fleet.Text(d["parentConsultation"]) != "" {
		depth := max(1, int(fleet.Number(d["consultationDepth"])))
		messages = messages[min(depth, len(messages)):]
	}
	if len(messages) > 0 && fleet.Object(messages[len(messages)-1])["role"] == "proxy" {
		evidence = fleet.Text(d["intakeSizeProxyEvidence"])
	}
	return evidence, messages
}

// The CLI carries the user's original statement and date inside its initial
// request. That message is available even before a consultation body is saved.
func inputSplitProhibited(d Record) bool {
	body, messages := noSplitEvidence(d)
	if supportedNoSplit(body, messages) {
		return true
	}
	for i, item := range messages {
		message := fleet.Object(item)
		text := fleet.Text(message["text"])
		if message["role"] != "user" || !strings.Contains(text, "## 利用者の判断と根拠") {
			continue
		}
		if i == 0 && !noSplitWithdrawn(messages[1:]) &&
			noSplitQuote(text, func(paragraph, quote string) bool {
				return len(sizeDate.FindAllStringSubmatch(paragraph, -1)) == 1
			}) {
			return true
		}
		if supportedNoSplit(text, messages) {
			return true
		}
	}
	return false
}

// bodyAcceptanceItems counts the bullets of the body's "## 受入条件" section,
// ignoring fenced examples, with the rules of filing.acceptanceCount.
func bodyAcceptanceItems(body string) int {
	inside, count := false, 0
	fence, fenceLen := byte(0), 0
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 3 {
			s := line[indent:]
			if len(s) > 0 && (s[0] == '`' || s[0] == '~') {
				n := 0
				for n < len(s) && s[n] == s[0] {
					n++
				}
				if n >= 3 {
					if fence != 0 {
						if s[0] == fence && n >= fenceLen && strings.Trim(s[n:], " \t") == "" {
							fence, fenceLen = 0, 0
						}
						continue
					}
					if s[0] != '`' || !strings.Contains(s[n:], "`") {
						fence, fenceLen = s[0], n
						continue
					}
				}
			}
		}
		if fence != 0 {
			continue
		}
		if !inside {
			inside = heavyAcceptanceStart.MatchString(line)
			continue
		}
		if heavyAcceptanceEnd.MatchString(line) {
			break
		}
		if heavyAcceptanceBullet.MatchString(line) {
			count++
		}
	}
	return count
}

// unsupportedHeavyReasons lists where a heavyReason would let a request with
// two or more separately mergeable acceptance criteria skip splitting without
// the user's quoted, dated no-split decision. A single criterion cannot be split,
// so its heavyReason is kept.
func unsupportedHeavyReasons(d, answer Record) []string {
	if answer["ready"] != true || fleet.Object(answer["newProject"])["needed"] == true || d["stallFix"] == true {
		return nil
	}
	// Hotfix reasons keep their existing size exemption (filing sizeExemption).
	checked := func(v any) bool {
		reason := strings.TrimSpace(fleet.Text(v))
		return reason != "" && !strings.HasPrefix(reason, "止血") && !strings.HasPrefix(reason, "有事")
	}
	locations := []string{}
	splits := fleet.Elements(answer["splitIssues"])
	if len(splits) == 0 {
		if checked(answer["heavyReason"]) && bodyAcceptanceItems(fleet.Text(answer["body"])) >= 2 {
			locations = append(locations, "本文")
		}
	}
	for i, raw := range splits {
		p := fleet.Object(raw)
		if checked(p["heavyReason"]) && len(fleet.Elements(p["acceptance"])) >= 2 {
			locations = append(locations, fmt.Sprintf("分割案%d", i+1))
		}
	}
	if len(locations) == 0 {
		return nil
	}
	if evidence, messages := noSplitEvidence(d); supportedNoSplit(evidence, messages) {
		return nil
	}
	return locations
}

// clearHeavyReasons removes unsupported heavyReason values so that filing does
// not raise the tier or mark the Issue as exempt from the PR size return.
func clearHeavyReasons(answer Record, locations []string) {
	for _, location := range locations {
		if location == "本文" {
			answer["heavyReason"] = ""
			continue
		}
		var index int
		if _, err := fmt.Sscanf(location, "分割案%d", &index); err == nil && index >= 1 {
			if splits := fleet.Elements(answer["splitIssues"]); index <= len(splits) {
				fleet.Object(splits[index-1])["heavyReason"] = ""
			}
		}
	}
}

func heavyReasonNotice(locations []string) string {
	return fmt.Sprintf("分けられない理由（heavyReason）が、利用者の分割禁止の発言（「## 利用者の判断と根拠」にある「」付きの原文引用と日時）で裏付けられていません（対象: %s）。受入条件が2項目以上あるため、目的・受入条件・依存を保ったまま、単独でマージできる挙動ごとの分割案を返してください。分ける必要がないと判断する場合は heavyReason を空文字にしてください。裏付けのない heavyReason は起票前に外します。", strings.Join(locations, "・"))
}

// checkHeavyReason asks once for a split when heavyReason lacks the user's
// quoted decision, then clears the unsupported values. It never blocks filing,
// so a request the AI still keeps whole is filed at the normal tier and stays
// subject to the size return. Persist before calling so a crash cannot loop.
func (r *readBudget) checkHeavyReason(ctx context.Context, d, answer Record, final bool, turnReads int) (bool, error) {
	locations := unsupportedHeavyReasons(d, answer)
	state := fleet.Object(r.value["heavyReason"])
	if len(locations) == 0 {
		if state != nil && state["pending"] == true {
			state["pending"] = false
			delete(state, "notice")
			return false, r.save(ctx)
		}
		return false, nil
	}
	if state == nil {
		state = Record{}
	}
	found := []any{}
	for _, location := range locations {
		found = append(found, location)
	}
	state["locations"] = found
	if fleet.Number(state["continuations"]) == 0 && !final && turnReads < 6 && !r.ending() && r.budget.check() == nil {
		state["continuations"] = 1
		state["requests"] = fleet.Number(state["requests"]) + 1
		state["pending"] = true
		state["notice"] = heavyReasonNotice(locations)
		r.value["rounds"] = fleet.Number(r.value["rounds"]) + 1
		r.value["answerPending"] = false
		r.value["heavyReason"] = state
		if err := r.saveContinuation(ctx, answer); err != nil {
			return false, err
		}
		return true, nil
	}
	clearHeavyReasons(answer, locations)
	state["pending"] = false
	state["cleared"] = found
	delete(state, "notice")
	r.value["heavyReason"] = state
	if err := r.save(ctx); err != nil {
		return false, err
	}
	answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n分けられない理由（heavyReason）は利用者の分割禁止の発言で裏付けられないため外しました（対象: " + strings.Join(locations, "・") + "）。難易度は受付の通常の判定で決まります。")
	return false, nil
}
