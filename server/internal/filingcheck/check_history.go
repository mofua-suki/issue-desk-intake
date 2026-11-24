package filingcheck

// checkHistoryLimit bounds persisted history independently of the current revision's count.
const checkHistoryLimit = 20

func boundedCheckHistory(checks []any) []any {
	if len(checks) > checkHistoryLimit {
		return append([]any(nil), checks[len(checks)-checkHistoryLimit:]...)
	}
	return checks
}
