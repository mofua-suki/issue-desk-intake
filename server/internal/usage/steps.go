package usage

import "issue-desk-intake/server/internal/intakestep"

// Intake step names are normalized only when interpreting usage, never when recording it.
const (
	ConsultationStep = "受付の相談"
	FilingStep       = "起票直前の重複と依存の確認"
	ProxyStep        = "代理の回答"

	ProxyConfirmationStep = "利用者への質問の代理確認"
	FilingCheckStep       = intakestep.FilingCheck

	DraftStep = intakestep.Draft
	// TierClassificationStep names usage rows that the removed tier classifier
	// recorded before the requester had to choose the tier. Nothing records it
	// any more; it stays so statistics can read the old rows.
	TierClassificationStep = "受付の始めの判定"
	FilingScopeStep        = "起票時の変更範囲の見積もり"
	ReviewNotesStep        = "注意点の更新"
)

func CanonicalStep(step string) string {
	switch step {
	case DraftStep:
		return ConsultationStep
	case FilingCheckStep:
		return FilingStep
	case ProxyConfirmationStep:
		return ProxyStep
	default:
		return step
	}
}

func IsIntakeStep(step string) bool {
	switch CanonicalStep(step) {
	case ConsultationStep, FilingStep, ProxyStep:
		return true
	}
	return false
}
