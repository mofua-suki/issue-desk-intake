package hosts

import (
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"regexp"
	"strconv"
	"time"
)

func DrainDefault() (time.Duration, error) {
	minutes := 150
	if raw, exists := os.LookupEnv("ISSUE_DESK_DRAIN_DEFAULT_MINUTES"); exists {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1440 {
			return 0, errors.New("invalid drain duration")
		}
		minutes = n
	}
	return time.Duration(minutes) * time.Minute, nil
}
func DefaultDuration() time.Duration { duration, _ := DrainDefault(); return duration }
func ParseDrainTime(value any) (time.Time, error) {
	s := fleet.Text(value)
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d(?::\d\d(?:\.\d{1,9})?)?(?:Z|[+-]\d\d:\d\d)$`).MatchString(s) {
		return time.Time{}, errors.New("invalid drain timestamp")
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		at, err = time.Parse("2006-01-02T15:04Z07:00", s)
	}
	return at, err
}
