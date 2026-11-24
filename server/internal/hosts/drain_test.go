package hosts

import (
	"testing"
	"time"
)

func TestDrainUsesConfiguredDefaultAndExplicitExpiry(t *testing.T) {
	t.Setenv("ISSUE_DESK_DRAIN_DEFAULT_MINUTES", "10")
	duration, err := DrainDefault()
	if err != nil || duration != 10*time.Minute {
		t.Fatal(duration, err)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mark := map[string]any{"at": "2026-11-01T00:00Z", "by": "operator"}
	if ActiveDrain(mark, at.Add(11*time.Minute), duration) != nil {
		t.Fatal("expired inferred drain retained")
	}
	mark["until"] = "2026-11-05T00:00+00:00"
	if ActiveDrain(mark, at.Add(24*time.Hour), duration) == nil {
		t.Fatal("explicit later expiry narrowed")
	}
	t.Setenv("ISSUE_DESK_DRAIN_DEFAULT_MINUTES", "0")
	if _, err = DrainDefault(); err == nil {
		t.Fatal("invalid drain setting accepted")
	}
}

func TestActiveDrainIndefiniteAndLegacy(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if ActiveDrain(map[string]any{"indefinite": true, "at": at.Format(time.RFC3339)}, at.Add(24*time.Hour), 150*time.Minute) == nil {
		t.Fatal("indefinite expired")
	}
	legacy := map[string]any{"at": at.Format(time.RFC3339)}
	if ActiveDrain(legacy, at.Add(149*time.Minute), 150*time.Minute) == nil || ActiveDrain(legacy, at.Add(151*time.Minute), 150*time.Minute) != nil {
		t.Fatal("legacy expiry")
	}
}
