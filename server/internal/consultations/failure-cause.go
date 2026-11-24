package consultations

import (
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"unicode/utf8"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
)

const unknownFailureStage = "相談の処理"

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// AtStage は失敗した処理の段階を原因に付ける。Error() と errors.As・errors.Is の結果は変えない。
// 既に段階を持つ失敗は、より内側の段階を残す。
func AtStage(stage string, err error) error {
	var known *stageError
	if err == nil || errors.As(err, &known) {
		return err
	}
	return &stageError{stage: stage, err: err}
}

// FailureStage は AtStage で付けた段階を返す。無ければ「相談の処理」。
func FailureStage(err error) string {
	var known *stageError
	if errors.As(err, &known) {
		return known.stage
	}
	return unknownFailureStage
}

const shortestConversationText = 16

// scrubConversation は原因の文に混ざった相談の会話・本文を除く。短い語は通常の語と区別できないので残す。
func scrubConversation(d Record, value string) string {
	texts := []string{text(d["body"])}
	for _, item := range fleet.Items(d["messages"]) {
		texts = append(texts, text(record(item)["text"]))
	}
	found := map[string]bool{}
	for _, whole := range texts {
		found[strings.TrimSpace(whole)] = true
		for _, line := range strings.Split(whole, "\n") {
			found[strings.TrimSpace(line)] = true
		}
	}
	ordered := make([]string, 0, len(found))
	for part := range found {
		if utf8.RuneCountInString(part) >= shortestConversationText {
			ordered = append(ordered, part)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, part := range ordered {
		value = strings.ReplaceAll(value, part, "[相談の本文]")
	}
	return value
}

// FailureCause は相談の記録に残せる形の原因を返す。会話・本文と資格情報を除き、1行・500文字以内にする。
func FailureCause(d Record, err error) string {
	if err == nil {
		return ""
	}
	return contract.LimitCause(contract.MaskCredentials(scrubConversation(d, contract.CauseText(err))))
}

// PublicFailure は相談の error に出す文を返す。最初から公開する文を持つ失敗はその文、
// それ以外は fallback に段階と原因を添える。
func PublicFailure(d Record, err error, fallback string) string {
	var public interface{ PublicMessage() string }
	if errors.As(err, &public) {
		return public.PublicMessage()
	}
	var problem *contract.Problem
	if errors.As(err, &problem) && problem != nil {
		return problem.Message
	}
	return fallback + "段階: " + FailureStage(err) + "。原因: " + FailureCause(d, err)
}

// FailureRecord は相談の lastFailure の形を返す。stage は処理の段階、cause は秘匿済みの元の原因、
// kind は受付AIの呼び出しの失敗の種類（ある場合だけ）。
func FailureRecord(at string, d Record, err error, message string) Record {
	out := Record{"at": at, "message": message, "stage": FailureStage(err), "cause": FailureCause(d, err)}
	var failure *ai.Failure
	if errors.As(err, &failure) && failure.Kind != "" {
		out["kind"] = failure.Kind
	}
	return out
}

func failureLogLine(id string, d Record, err error) string {
	line := "相談の処理に失敗しました id=" + id + " stage=" + FailureStage(err)
	var failure *ai.Failure
	if errors.As(err, &failure) && failure.Kind != "" {
		line += " kind=" + failure.Kind
	}
	return line + " cause=" + FailureCause(d, err)
}
