package contract

import (
	"testing"
	"time"
)

func TestServerTiming(t *testing.T) {
	if got := ServerTiming(TimingMetric{"read", 1234567 * time.Nanosecond}, TimingMetric{"page", 2 * time.Millisecond}, TimingMetric{"view", 0}); got != "read;dur=1.235, page;dur=2.000, view;dur=0.000" {
		t.Fatalf("unexpected header: %q", got)
	}
	if got := ServerTiming(); got != "" {
		t.Fatalf("empty metrics: %q", got)
	}
}
