package hosts

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"time"
)

type loadSample struct {
	At   time.Time
	Load map[string]any
}

func (s *Service) loadHistory(ctx context.Context, now time.Time) (map[string][]loadSample, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT host,received_at,load FROM host_load_samples WHERE received_at>=$1 AND received_at<=$2", now.Add(-3*time.Hour), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := map[string][]loadSample{}
	for rows.Next() {
		var host string
		var at time.Time
		var raw []byte
		if err = rows.Scan(&host, &at, &raw); err != nil {
			return nil, err
		}
		var load map[string]any
		if len(raw) > 0 && json.Unmarshal(raw, &load) != nil {
			continue
		}
		samples[host] = append(samples[host], loadSample{at, load})
	}
	return samples, rows.Err()
}
func figure(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func loadSummary(samples []loadSample) any {
	if len(samples) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"cpuPercent", "freeMemoryMB", "freeDiskMB", "loadAverage1m", "logicalCpuCount", "gpuPercent", "usedVramMB", "freeVramMB"} {
		sum, max, count := 0.0, 0.0, 0
		for _, sample := range samples {
			if n, ok := figure(sample.Load[key]); ok {
				sum += n
				if n > max {
					max = n
				}
				count++
			}
		}
		out[key] = nil
		if count > 0 {
			out[key] = map[string]any{"max": max, "average": sum / float64(count), "count": count}
		}
	}
	return out
}
func overloaded(samples []loadSample, now time.Time, limit map[string]any, leaseMinutes float64) bool {
	total := 0
	latest := time.Time{}
	for _, sample := range samples {
		if sample.At.Before(now.Add(-time.Duration(fleet.Number(limit["minutes"]))*time.Minute)) || sample.At.After(now) {
			continue
		}
		total++
		if sample.At.After(latest) {
			latest = sample.At
		}
		load := sample.Load
		if value, exists := load["loadAverage1m"]; exists && value != nil {
			n, nOK := figure(value)
			cores, coresOK := figure(load["logicalCpuCount"])
			if !nOK || !coresOK || cores == 0 || math.Trunc(cores) != cores || n/cores <= fleet.Number(limit["ratio"]) {
				return false
			}
		} else {
			n, ok := figure(load["cpuPercent"])
			if !ok || n > 100 || n < fleet.Number(limit["cpuPercent"]) {
				return false
			}
		}
	}
	return total >= 2 && now.Sub(latest) <= time.Duration(leaseMinutes)*time.Minute
}
