package consultations

import (
	"context"
	"database/sql"
	"strings"
)

type cachedDraft struct {
	version string
	record  Record
	bytes   int
}

type draftVersion struct{ key, version string }

// PostgreSQL's extended protocol accepts at most 65535 bind parameters.
const draftParameterLimit = 65535

// scanDraftRows owns rows and preserves the first scan/decode/iteration error
// when closing also fails. Both the key list and projection use this path.
func scanDraftRows(rows *sql.Rows, scan func() error) (err error) {
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		if err = scan(); err != nil {
			return err
		}
	}
	return rows.Err()
}

// A repeatable-read snapshot keeps the version list and projections consistent
// even when another connection updates or deletes a row between the queries.
func (s *Records) readCachedDraftEntries(ctx context.Context, purpose DraftReadPurpose, projection string) (result []DraftFields, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// One gate retains serialization across purposes and protects the shared map.
	// Initialization never holds the gate or waits for database work.
	s.draftOnce.Do(func() { s.draftGate = make(chan struct{}, 1) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.draftGate <- struct{}{}:
	}
	defer func() { <-s.draftGate }()
	// Cancellation and acquisition can become ready at the same time.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.draftMetricMissing(purpose)
		}
	}()
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id, xmin::text FROM drafts ORDER BY "+rowOrder(true))
	if err != nil {
		return nil, err
	}
	versions := make([]draftVersion, 0)
	err = scanDraftRows(rows, func() error {
		var v draftVersion
		if err := rows.Scan(&v.key, &v.version); err != nil {
			return err
		}
		versions = append(versions, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	old := s.draftCache[purpose]
	next, changed := reusableCachedDrafts(old, versions)
	if len(changed) > 0 {
		// Fetch all projections on the first read or beyond the bind limit.
		// Keep both reads in this transaction for the same snapshot.
		query, args := changedDraftsQuery(projection, changed, len(versions))
		rows, err = tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		err = scanDraftRows(rows, func() error {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				return err
			}
			record, err := decode(raw)
			if err != nil {
				return err
			}
			next[key] = cachedDraft{record: record, bytes: len(key) + len(raw)}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result, totalBytes, err := cachedDraftEntries(versions, next)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if s.draftCache == nil {
		s.draftCache = make(map[DraftReadPurpose]map[string]cachedDraft)
	}
	s.draftCache[purpose] = next
	s.draftMetricSuccess(purpose, len(next), totalBytes, old == nil || len(changed) > 0 || len(old) != len(next))
	return result, nil
}

// reusableCachedDrafts keeps the cached projections whose row version is unchanged and
// lists the keys that must be read again.
func reusableCachedDrafts(old map[string]cachedDraft, versions []draftVersion) (map[string]cachedDraft, []string) {
	next := make(map[string]cachedDraft, len(versions))
	changed := make([]string, 0)
	for _, v := range versions {
		if item, ok := old[v.key]; ok && item.version == v.version {
			next[v.key] = item
		} else {
			changed = append(changed, v.key)
		}
	}
	return next, changed
}

// changedDraftsQuery limits the projection query to the changed rows unless every row
// changed or the keys exceed the bind limit.
func changedDraftsQuery(projection string, changed []string, total int) (string, []any) {
	query := projection
	var args []any
	if len(changed) != total && len(changed) <= draftParameterLimit {
		marks := make([]string, len(changed))
		args = make([]any, len(changed))
		for i, key := range changed {
			marks[i] = "?"
			args[i] = key
		}
		query = strings.Replace(query, " FROM drafts ORDER BY ", " FROM drafts WHERE id IN ("+strings.Join(marks, ",")+") ORDER BY ", 1)
		query = bindSQL(query)
	}
	return query, args
}

// cachedDraftEntries lists copies of the projections in version order, stamps the cache
// entries with their versions and totals their size.
func cachedDraftEntries(versions []draftVersion, next map[string]cachedDraft) ([]DraftFields, int, error) {
	result := make([]DraftFields, 0, len(versions))
	totalBytes := 0
	for _, v := range versions {
		item, ok := next[v.key]
		if !ok {
			return nil, 0, sql.ErrNoRows
		}
		item.version = v.version
		totalBytes += item.bytes
		next[v.key] = item
		result = append(result, DraftFields{Key: v.key, Record: copyDraftRecord(item.record)})
	}
	return result, totalBytes, nil
}

func copyDraftValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(x))
		for k, value := range x {
			result[k] = copyDraftValue(value)
		}
		return result
	case Record:
		return copyDraftRecord(x)
	case []any:
		result := make([]any, len(x))
		for i, value := range x {
			result[i] = copyDraftValue(value)
		}
		return result
	default:
		return v
	}
}
func copyDraftRecord(record Record) Record {
	result := make(Record, len(record))
	for key, value := range record {
		result[key] = copyDraftValue(value)
	}
	return result
}
