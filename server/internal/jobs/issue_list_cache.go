package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// issueListCache keeps the decoded jobs of the issue list between reads. Each
// read lists every row's id with xmin, the transaction that wrote the current
// row version, and decodes only the rows whose version is not cached.
type issueListCache struct {
	// gate serializes reads and protects entries and metric. A waiting reader
	// leaves when its context ends.
	gate    chan struct{}
	entries map[int64]cachedIssueJob
	metric  issueListMetric
}

type cachedIssueJob struct {
	version int64
	job     *Job
	bytes   int
}

type issueJobVersion struct{ id, version int64 }

func newIssueListCache() *issueListCache {
	return &issueListCache{gate: make(chan struct{}, 1)}
}

// AllForIssues loads the job projections used by an issue list. The returned
// jobs are shared with later calls: callers must treat every job, including
// its View, Result and Scope, as read-only.
func (s *Store) AllForIssues(ctx context.Context) ([]*Job, error) {
	if s.issueList == nil {
		return readAllJobs(ctx, s.db)
	}
	return s.issueList.read(ctx, s.db, s.options.Now)
}

// readAllJobs decodes every row. It is the read of a store without a cache.
func readAllJobs(ctx context.Context, db *sql.DB) ([]*Job, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs ORDER BY id")
	if err != nil {
		return nil, err
	}
	var result []*Job
	err = scanJobRows(rows, func() error {
		job, err := scanJob(rows)
		if err != nil {
			return err
		}
		result = append(result, job)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// scanJobRows owns rows and keeps the first scan, decode or iteration error
// when closing also fails.
func scanJobRows(rows *sql.Rows, scan func() error) (err error) {
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

// read returns every job in id order. A repeatable-read snapshot keeps the
// version list and the rows consistent when another connection updates or
// deletes a row between the two queries. A failed read leaves the cache as it was.
func (c *issueListCache) read(ctx context.Context, db *sql.DB, now func() time.Time) (result []*Job, err error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.gate <- struct{}{}:
	}
	defer func() { <-c.gate }()
	// Cancellation and acquisition can become ready at the same time.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.observeMissing(now())
		}
	}()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id, xmin::text::bigint FROM jobs ORDER BY id")
	if err != nil {
		return nil, err
	}
	versions := make([]issueJobVersion, 0, len(c.entries))
	err = scanJobRows(rows, func() error {
		var v issueJobVersion
		if err := rows.Scan(&v.id, &v.version); err != nil {
			return err
		}
		versions = append(versions, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Rows missing from the version list were deleted and are not carried over.
	next := make(map[int64]cachedIssueJob, len(versions))
	changed := map[int64]int64{}
	for _, v := range versions {
		if item, ok := c.entries[v.id]; ok && item.version == v.version {
			next[v.id] = item
		} else {
			changed[v.id] = v.version
		}
	}
	if len(changed) > 0 {
		query, args, err := changedJobsQuery(changed, len(versions))
		if err != nil {
			return nil, err
		}
		if rows, err = tx.QueryContext(ctx, query, args...); err != nil {
			return nil, err
		}
		err = scanJobRows(rows, func() error {
			job, size, err := scanJobSized(rows)
			if err != nil {
				return err
			}
			if version, ok := changed[job.ID]; ok {
				next[job.ID] = cachedIssueJob{version: version, job: job, bytes: size}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result = make([]*Job, 0, len(versions))
	total := 0
	for _, v := range versions {
		item, ok := next[v.id]
		if !ok {
			return nil, sql.ErrNoRows
		}
		total += item.bytes
		result = append(result, item.job)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	c.entries = next
	c.observeSuccess(now(), len(next), total, len(changed))
	return result, nil
}

// changedJobsQuery limits the row read to the changed ids unless every row changed.
func changedJobsQuery(changed map[int64]int64, total int) (string, []any, error) {
	query := "SELECT " + jobColumns + " FROM jobs"
	if len(changed) == total {
		return query, nil, nil
	}
	ids := make([]int64, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return "", nil, err
	}
	return query + " WHERE id IN (SELECT value::bigint FROM jsonb_array_elements_text($1::jsonb))", []any{string(data)}, nil
}

// issueListMetricInterval bounds how often a working cache logs: running jobs
// renew their lease, so most reads decode a few rows again.
const issueListMetricInterval = 10 * time.Minute

// issueListMetric logs the size of the cache and how much it read again. A
// success logs count (cached jobs), logical_bytes (UTF-8 bytes of the repo,
// title and JSON columns as last read, not Go heap usage), and reads and
// reread_rows since the previous success event. It is written on the first
// read, on recovery and at most once per issueListMetricInterval. A failure
// logs status=missing once until the next success.
type issueListMetric struct {
	initialized bool
	missing     bool
	loggedAt    time.Time
	reads       int
	reread      int
}

func (c *issueListCache) observeMissing(at time.Time) {
	if !c.metric.initialized || !c.metric.missing {
		slog.Info("job list cache observation", "status", "missing", "measured_at", at.UTC())
	}
	c.metric.initialized, c.metric.missing = true, true
}

func (c *issueListCache) observeSuccess(at time.Time, count, bytes, reread int) {
	m := &c.metric
	m.reads++
	m.reread += reread
	if m.initialized && !m.missing && at.Sub(m.loggedAt) < issueListMetricInterval {
		return
	}
	slog.Info("job list cache observation", "status", "success", "count", count, "logical_bytes", bytes, "reads", m.reads, "reread_rows", m.reread, "measured_at", at.UTC())
	*m = issueListMetric{initialized: true, loggedAt: at}
}
