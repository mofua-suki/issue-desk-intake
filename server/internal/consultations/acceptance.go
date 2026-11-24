package consultations

func acceptance(at string, source any) Record {
	return Record{"waitStartedAt": at, "waitSource": source, "attempts": 0, "retryAt": nil, "lastFailure": nil, "timeoutStreak": 0, "failureRetry": false, "progress": nil, "carriedContext": nil, "tierRaised": false, "carriedRounds": 0}
}
