package contract

import (
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MeaningfulApproval reports whether statement is a concrete approval from the
// user: at least 8 UTF-16 units and not only a boilerplate word such as 「承認します」.
// The same rule is in shared/src/approval-statement.ts for the chat scripts.
func MeaningfulApproval(statement string) bool {
	statement = strings.TrimSpace(statement)
	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFKC.String(statement))
	if UTF16Length(statement) < 8 {
		return false
	}
	for _, v := range []string{"承認", "承認します", "approve", "approved", "yes", "ok", "了解", "了解しました", "お願いします", "よろしくお願いします", "どうぞよろしくお願いします", "お願いいたします", "よろしくお願いいたします", "どうぞよろしくお願いいたします"} {
		if clean == v {
			return false
		}
	}
	return true
}

// InstructionAtProblem is the refusal when a chat approval lacks a valid instruction time.
const InstructionAtProblem = "利用者がチャットで指示した日時（instructionAt: UTCオフセット付きISO 8601、未来でなく7日以内）が必要です。"

// InstructionAt validates the time of the user's chat statement that a chat
// approval relays: RFC 3339 with a UTC offset, not later than now and not older
// than 7 days. It returns the value as given so the user's offset is recorded.
// The same rule is in shared/src/approval-statement.ts for the chat scripts.
func InstructionAt(value any, now time.Time) (string, bool) {
	text, ok := value.(string)
	if !ok || len(text) > 64 {
		return "", false
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || at.After(now) || at.Before(now.Add(-7*24*time.Hour)) {
		return "", false
	}
	return text, true
}
