package statistics

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type logLine struct {
	At   time.Time
	Body string
}

var logStamp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z) (.*)$`)

func parseLogLines(text string) []logLine {
	lines := []logLine{}
	for _, line := range strings.Split(text, "\n") {
		m := logStamp.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, m[1])
		if e == nil {
			lines = append(lines, logLine{at, m[2]})
		}
	}
	return lines
}
func (s *Service) logs(ctx context.Context) ([]logLine, bool, time.Time, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT at,line FROM runner_log_lines WHERE host='nas' AND at<=$1 ORDER BY at,key", now)
	if e != nil {
		return nil, false, time.Time{}, e
	}
	lines := []logLine{}
	for rows.Next() {
		var at time.Time
		var body string
		if e = rows.Scan(&at, &body); e != nil {
			rows.Close()
			return nil, false, time.Time{}, e
		}
		if m := logStamp.FindStringSubmatch(body); m != nil {
			body = m[2]
		}
		lines = append(lines, logLine{at, body})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, false, time.Time{}, e
	}
	if len(lines) == 0 && s.RuntimeDir != "" {
		for _, name := range []string{"runner.log.previous", "runner.log"} {
			raw, e := os.ReadFile(filepath.Join(s.RuntimeDir, name))
			if e != nil && !os.IsNotExist(e) {
				return nil, false, time.Time{}, e
			}
			lines = append(lines, parseLogLines(string(raw))...)
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At.Before(lines[j].At) })
	var heartbeat sql.NullTime
	e = s.DB.QueryRowContext(ctx, "SELECT heartbeat_at FROM hosts WHERE host='nas'").Scan(&heartbeat)
	if e != nil && e != sql.ErrNoRows {
		return nil, false, time.Time{}, e
	}
	alive := heartbeat.Valid && !heartbeat.Time.Before(now.Add(-15*time.Minute))
	end := now
	if !alive {
		end = time.Time{}
		if len(lines) > 0 {
			end = lines[len(lines)-1].At
		}
		if heartbeat.Valid && heartbeat.Time.After(end) {
			end = heartbeat.Time
		}
		if end.After(now) {
			end = now
		}
	}
	return lines, alive, end, nil
}
