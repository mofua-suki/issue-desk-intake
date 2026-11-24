package anomalies

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Service struct {
	DB           *sql.DB
	Weights      map[string]any
	Now          func() time.Time
	Repositories []string
	FilingCheck  func(context.Context, string, time.Time) (map[string]any, error)
}

var zoned = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d{1,3})?)?(Z|[+-]\d{2}:\d{2})$`)

func Range(query url.Values, now time.Time) (time.Time, time.Time, error) {
	end := now
	var e error
	parse := func(value string) (time.Time, error) {
		if !zoned.MatchString(value) {
			return time.Time{}, contract.NewProblem(400, "from・to は時差付きのISO日時にしてください。", "")
		}
		if value[16] != ':' {
			value = value[:16] + ":00" + value[16:]
		}
		return time.Parse(time.RFC3339Nano, value)
	}
	if values, exists := query["to"]; exists {
		if len(values) != 1 {
			return time.Time{}, time.Time{}, contract.NewProblem(400, "toは1件だけ指定してください。", "")
		}
		end, e = parse(values[0])
		if e != nil {
			return time.Time{}, time.Time{}, contract.NewProblem(400, "toは時差付きのISO日時にしてください。", "")
		}
	}
	start := end.Add(-24 * time.Hour)
	if values, exists := query["from"]; exists {
		if len(values) != 1 {
			return time.Time{}, time.Time{}, contract.NewProblem(400, "fromは1件だけ指定してください。", "")
		}
		start, e = parse(values[0])
		if e != nil {
			return time.Time{}, time.Time{}, contract.NewProblem(400, "fromは時差付きのISO日時にしてください。", "")
		}
	}
	if !start.Before(end) || end.Sub(start) > 7*24*time.Hour {
		return time.Time{}, time.Time{}, contract.NewProblem(400, "fromはtoより前、期間は7日以内にしてください。", "")
	}
	return start, end, nil
}
func (s *Service) Detect(ctx context.Context, from, to time.Time) (map[string]any, error) {
	store := usage.Store{DB: s.DB}
	lower := to.Add(-21 * 24 * time.Hour)
	if from.Add(-14 * 24 * time.Hour).Before(lower) {
		lower = from.Add(-14 * 24 * time.Hour)
	}
	all, _, e := store.Read(ctx, "", lower, to.Add(time.Millisecond))
	if e != nil {
		return nil, e
	}
	selected, history, wanted := selectDetectRows(all, from, to)
	history, e = readOlderHistory(ctx, s.DB, wanted, lower, history)
	if e != nil {
		return nil, e
	}
	hits := Basic(selected, history, s.Weights)
	hits = append(hits, Cache(selected, history)...)
	hits = append(hits, CallTriggers(selected, history)...)
	hits = append(hits, UnitTotals(selected, history, s.Weights)...)
	hits = append(hits, ProviderShifts(selected, history)...)
	hits = uniqueCauses(hits)
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	repeat := IntakeRepeats(all, to, now)
	hits = append(hits, intakeRepeatHits(repeat, to)...)
	limitRows, readReason := s.readLimitRows(ctx, to)
	forecast := LimitForecast(limitRows, to, readReason)
	for _, r := range forecast {
		if r["status"] == "hit" {
			hits = append(hits, r)
		}
	}
	filing := []map[string]any{}
	if s.FilingCheck != nil {
		for _, repo := range s.Repositories {
			read, e := s.FilingCheck(ctx, repo, to)
			r := FilingCheckResult(repo, read, e != nil, to)
			filing = append(filing, r)
			if r["status"] == "hit" {
				hits = append(hits, r)
			}
		}
	}
	cache := FilingCaches(all, to, s.Weights)
	for _, r := range cache {
		if r["status"] == "hit" {
			hits = append(hits, r)
		}
	}
	return map[string]any{"filingCheck": filing, "filingCache": cache, "limitForecast": forecast, "intakeRepeat": repeat, "from": iso(from), "to": iso(to), "rows": len(selected), "anomalies": hits}, nil
}

// selectDetectRows separates the rows under inspection from the comparison
// history, and names the issues whose older history must also be read.
func selectDetectRows(all []map[string]any, from, to time.Time) (selected, history []map[string]any, wanted map[string]map[string]any) {
	selected, history = []map[string]any{}, []map[string]any{}
	wanted = map[string]map[string]any{}
	for _, r := range all {
		t := at(r)
		if !t.Before(from) && t.Before(to) {
			selected = append(selected, r)
			if key := issueKey(r); key != "" {
				wanted[key] = r
			}
		}
	}
	for _, r := range all {
		t := at(r)
		if t.Before(to) && (!t.Before(from.Add(-14*24*time.Hour)) || wanted[issueKey(r)] != nil) {
			history = append(history, r)
		}
	}
	return selected, history, wanted
}

// uniqueCauses keeps the first hit of each cause.
func uniqueCauses(hits []map[string]any) []map[string]any {
	seen := map[string]bool{}
	dedup := []map[string]any{}
	for _, h := range hits {
		key := h["causeKey"].(string)
		if !seen[key] {
			dedup = append(dedup, h)
			seen[key] = true
		}
	}
	return dedup
}

// intakeRepeatHits turns confirmed and provisional intake repeat results into hits.
func intakeRepeatHits(repeat []map[string]any, to time.Time) []map[string]any {
	hits := []map[string]any{}
	for _, r := range repeat {
		if r["status"] == "hit" {
			h := map[string]any{}
			for k, v := range r {
				h[k] = v
			}
			h["provisional"] = false
			hits = append(hits, h)
		}
		if p := r["provisional"].(map[string]any); p["status"] == "hit" {
			h := map[string]any{}
			for k, v := range r {
				h[k] = v
			}
			for k, v := range p {
				h[k] = v
			}
			h["provisional"], h["at"] = true, iso(to)
			hits = append(hits, h)
		}
	}
	return hits
}

// readLimitRows reads the last eight days of rate limit rows. A read failure is
// returned as the reason the forecast is held, not as an error.
func (s *Service) readLimitRows(ctx context.Context, to time.Time) ([]map[string]any, string) {
	limitRows := []map[string]any{}
	readReason := ""
	rows, e := s.DB.QueryContext(ctx, "SELECT json FROM usage_limit_rows WHERE at>$1 AND at<=$2 ORDER BY at,key", to.Add(-8*24*time.Hour), to)
	if e != nil {
		readReason = "read_limits_failed"
	} else {
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				readReason = "read_limits_failed"
				break
			}
			var r map[string]any
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			limitRows = append(limitRows, r)
		}
		if rows.Err() != nil {
			readReason = "read_limits_failed"
		}
		rows.Close()
	}
	return limitRows, readReason
}

func olderHistoryQuery(targets []map[string]any, lower time.Time) (string, []any) {
	args := []any{lower}
	pairs := make([]string, 0, len(targets))
	for _, target := range targets {
		pairs = append(pairs, fmt.Sprintf("(repo=$%d AND issue=$%d)", len(args)+1, len(args)+2))
		args = append(args, target["repo"], target["issue"])
	}
	return "SELECT json FROM usage_rows WHERE at<$1 AND (" + strings.Join(pairs, " OR ") + ") ORDER BY at,key", args
}

func readOlderHistory(ctx context.Context, db *sql.DB, wanted map[string]map[string]any, lower time.Time, history []map[string]any) ([]map[string]any, error) {
	targets := make([]map[string]any, 0, len(wanted))
	for _, target := range wanted {
		targets = append(targets, target)
	}
	for start := 0; start < len(targets); start += 256 {
		end := min(start+256, len(targets))
		query, args := olderHistoryQuery(targets[start:end], lower)
		older, e := db.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		rows, _, e := appsql.JSONObjects[[]byte](older, appsql.JSONRowsOptions{})
		if e != nil {
			return nil, e
		}
		history = append(history, rows...)
	}
	return history, nil
}
