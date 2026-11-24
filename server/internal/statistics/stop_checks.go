package statistics

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

type stopCheckLedgerEntry struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Purpose      string `json:"purpose"`
	IntroducedAt string `json:"introducedAt"`
	NextReviewAt string `json:"nextReviewAt"`
	Counted      bool   `json:"counted"`
	Reviews      []struct {
		Date   string `json:"date"`
		Result string `json:"result"`
	} `json:"reviews"`
}

type stopCheckRow struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Purpose      string     `json:"purpose"`
	IntroducedAt string     `json:"introducedAt"`
	NextReviewAt string     `json:"nextReviewAt"`
	Overdue      bool       `json:"overdue"`
	Total        *int64     `json:"total"`
	Last30Days   *int64     `json:"last30Days"`
	Blocked      *int64     `json:"blocked"`
	Warned       *int64     `json:"warned"`
	LastFiredAt  *time.Time `json:"lastFiredAt"`
	ReviewResult *string    `json:"reviewResult"`
}

type stopCheckAggregate struct {
	total, recent, blocked, warned int64
	last                           sql.NullTime
}

func (s *Service) StopChecks(ctx context.Context) (any, error) {
	if stopChecksLoadErr != nil {
		return nil, stopChecksLoadErr
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT check_id, count(*), count(*) FILTER (WHERE occurred_at >= $1 AND occurred_at <= $2), count(*) FILTER (WHERE result = 'block'), count(*) FILTER (WHERE result = 'warn'), max(occurred_at) FROM stop_check_records GROUP BY check_id`, now.Add(-30*24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aggregates := map[string]stopCheckAggregate{}
	for rows.Next() {
		var id string
		var a stopCheckAggregate
		if err := rows.Scan(&id, &a.total, &a.recent, &a.blocked, &a.warned, &a.last); err != nil {
			return nil, err
		}
		aggregates[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buildStopChecks(stopCheckLedger, aggregates, now), nil
}

func buildStopChecks(ledger []stopCheckLedgerEntry, aggregates map[string]stopCheckAggregate, now time.Time) map[string]any {
	today := now.In(JST).Format("2006-01-02")
	result := make([]stopCheckRow, 0, len(ledger))
	known := make(map[string]bool, len(ledger))
	for _, entry := range ledger {
		known[entry.ID] = true
		row := stopCheckRow{ID: entry.ID, Kind: entry.Kind, Purpose: entry.Purpose, IntroducedAt: entry.IntroducedAt, NextReviewAt: entry.NextReviewAt, Overdue: today > entry.NextReviewAt}
		latest := ""
		for _, review := range entry.Reviews {
			if review.Date >= latest {
				latest = review.Date
				value := review.Result
				row.ReviewResult = &value
			}
		}
		if entry.Counted {
			a := aggregates[entry.ID]
			row.Total, row.Last30Days, row.Blocked, row.Warned = &a.total, &a.recent, &a.blocked, &a.warned
			if a.last.Valid {
				at := a.last.Time.UTC()
				row.LastFiredAt = &at
			}
		}
		result = append(result, row)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Overdue && !result[j].Overdue })
	unknown := []string{}
	for id := range aggregates {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	return map[string]any{"rows": result, "unknownCheckIds": unknown}
}
