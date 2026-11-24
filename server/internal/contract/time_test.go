package contract

import (
	"testing"
	"time"
)

func TestISO(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"zero milliseconds", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.000Z"},
		{"truncates below milliseconds", time.Date(2026, 1, 2, 3, 4, 5, 123987654, time.UTC), "2026-01-02T03:04:05.123Z"},
		{"converts other zones to UTC", time.Date(2026, 1, 2, 8, 4, 5, 7000000, tokyo), "2026-01-01T23:04:05.007Z"},
	}
	for _, c := range cases {
		if got := ISO(c.at); got != c.want {
			t.Errorf("%s: ISO = %q, want %q", c.name, got, c.want)
		}
	}
	if got, err := time.Parse(TimeLayout, "2026-01-01T23:04:05.007Z"); err != nil || !got.Equal(time.Date(2026, 1, 2, 8, 4, 5, 7000000, tokyo)) {
		t.Errorf("TimeLayout does not parse its own output: %v %v", got, err)
	}
}
