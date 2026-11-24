package contract

import (
	"fmt"
	"strings"
	"time"
)

// TimingMetric describes one Server-Timing duration.
type TimingMetric struct {
	Name     string
	Duration time.Duration
}

// ServerTiming formats durations in milliseconds for the Server-Timing header.
func ServerTiming(metrics ...TimingMetric) string {
	parts := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		parts = append(parts, fmt.Sprintf("%s;dur=%.3f", metric.Name, float64(metric.Duration)/float64(time.Millisecond)))
	}
	return strings.Join(parts, ", ")
}
