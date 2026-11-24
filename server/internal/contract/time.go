package contract

import "time"

// TimeLayout is the UTC millisecond timestamp format used by the API and stored records.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// ISO formats t in UTC with TimeLayout.
func ISO(t time.Time) string { return t.UTC().Format(TimeLayout) }
