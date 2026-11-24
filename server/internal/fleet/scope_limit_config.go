package fleet

// ScopeLimitFiles returns the explicit intakeSplit.files before shared default
// completion. Missing, invalid or unavailable values use the widening fallback.
func (a *Assignments) ScopeLimitFiles() int {
	if a == nil || a.scopeLimitFiles <= 0 {
		return 8
	}
	return a.scopeLimitFiles
}
