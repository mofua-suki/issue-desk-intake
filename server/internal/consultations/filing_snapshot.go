package consultations

import (
	"context"
	"database/sql"
	"errors"
)

// ReadFilingSnapshot selects full drafts from the same snapshot as their projections.
// The selector receives database keys separately from the stored JSON IDs.
func (s *Records) ReadFilingSnapshot(ctx context.Context, selectKeys func([]DraftFields) []string) ([]Record, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query, err := draftProjectionSQL(DraftReadFiling)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT filing_projection"); err != nil {
		return nil, err
	}
	entries, err := filingSnapshotEntries(ctx, tx, query)
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "22P02" || state.SQLState() == "22P05") {
		if _, e := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT filing_projection"); e != nil {
			return nil, e
		}
		entries, err = filingSnapshotEntries(ctx, tx, "SELECT id, json FROM drafts ORDER BY "+rowOrder(true))
	}
	if err != nil {
		return nil, err
	}
	ids := selectKeys(entries)
	full := map[string]Record{}
	if len(ids) > 0 {
		query, args := selectedDraftsSQL(ids)
		// Include keys because JSON IDs can be absent or differ from database keys.
		query = "SELECT id, " + query[len("SELECT "):]
		rows, e := tx.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var key, raw string
			if e = rows.Scan(&key, &raw); e != nil {
				break
			}
			var record Record
			record, e = decode(raw)
			if e != nil {
				break
			}
			full[key] = record
		}
		if e == nil {
			e = rows.Err()
		}
		if closeErr := rows.Close(); e == nil {
			e = closeErr
		}
		if e != nil {
			return nil, e
		}
	}
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if record, ok := full[entry.Key]; ok {
			result = append(result, record)
		} else {
			result = append(result, entry.Record)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func filingSnapshotEntries(ctx context.Context, tx *sql.Tx, query string) ([]DraftFields, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []DraftFields{}
	for rows.Next() {
		var key, raw string
		if err = rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		record, e := decode(raw)
		if e != nil {
			return nil, e
		}
		entries = append(entries, DraftFields{Key: key, Record: record})
	}
	return entries, rows.Err()
}
