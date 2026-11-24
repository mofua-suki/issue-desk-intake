package ai

import "regexp"

var sessionID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func sessionFrom(raw []byte) string {
	for _, row := range events(raw) {
		if row["type"] != "thread.started" && row["type"] != "system" {
			continue
		}
		for _, key := range []string{"thread_id", "session_id"} {
			if id, ok := row[key].(string); ok && sessionID.MatchString(id) {
				return id
			}
		}
	}
	return ""
}
func SessionCost(usage Usage, base *float64, resumed, other bool) Usage {
	total := usage.CostUSD
	usage.SessionCostUSD = total
	reason := ""
	if total == nil {
		reason = "no-total"
	} else if other {
		reason = "other-session"
	} else if resumed && base == nil {
		reason = "no-base"
	} else if resumed && *total < *base {
		reason = "total-decreased"
	}
	if reason != "" {
		usage.CostUSD = nil
		usage.CostUnknownReason = &reason
		return usage
	}
	if resumed {
		delta := *total - *base
		usage.CostUSD = &delta
	}
	return usage
}
