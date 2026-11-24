package statistics

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

//go:embed stop-checks.json
var stopChecksJSON []byte

var stopCheckLedger, countedStopChecks, stopChecksLoadErr = loadStopCheckLedger(stopChecksJSON)

func loadStopCheckLedger(data []byte) ([]stopCheckLedgerEntry, map[string]struct{}, error) {
	var ledger []stopCheckLedgerEntry
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, nil, fmt.Errorf("load stop check ledger: %w", err)
	}
	ids := make(map[string]struct{})
	for _, entry := range ledger {
		if entry.Counted && entry.ID != "" {
			ids[entry.ID] = struct{}{}
		}
	}
	return ledger, ids, nil
}

var stopCheckName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var stopCheckTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
var stopCheckUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var ErrInvalidStopCheck = errors.New("invalid stop check record")

type invalidStopCheck string

func (e invalidStopCheck) Error() string        { return string(e) }
func (e invalidStopCheck) Is(target error) bool { return target == ErrInvalidStopCheck }

type StopCheckExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// StopCheckRecord contains only the fields needed to count a hook firing.
// Host must come from the authenticated sender, not the request body.
type StopCheckRecord struct {
	ID         string
	CheckID    string
	Result     string
	OccurredAt string
	Project    string
	Host       string
}

// validateStopCheck checks the embedded ledger and normalizes dimensions.
// Invalid project and host names are replaced with other. Times must include a timezone.
func validateStopCheck(record StopCheckRecord) (StopCheckRecord, time.Time, error) {
	if stopChecksLoadErr != nil {
		return record, time.Time{}, stopChecksLoadErr
	}
	if _, known := countedStopChecks[record.CheckID]; !known {
		return record, time.Time{}, invalidStopCheck("unknown stop check ID")
	}
	if !stopCheckUUID.MatchString(record.ID) {
		return record, time.Time{}, invalidStopCheck("invalid record ID")
	}
	if record.Result != "block" && record.Result != "warn" {
		return record, time.Time{}, invalidStopCheck("invalid stop check result")
	}
	at, err := time.Parse(time.RFC3339Nano, record.OccurredAt)
	if err != nil || !stopCheckTime.MatchString(record.OccurredAt) || at.Year() < 1 || at.Year() > 9999 {
		return record, time.Time{}, invalidStopCheck("invalid occurrence time")
	}
	if !stopCheckName.MatchString(record.Project) {
		record.Project = "other"
	}
	if !stopCheckName.MatchString(record.Host) {
		record.Host = "other"
	}
	return record, at, nil
}

const stopCheckChunkSize = 200

// WriteStopCheckRecords validates all records before writing. The caller must
// supply a transaction and roll it back on error to make all chunks atomic.
func WriteStopCheckRecords(ctx context.Context, tx StopCheckExecutor, records []StopCheckRecord) error {
	type validated struct {
		record StopCheckRecord
		at     time.Time
	}
	rows := make([]validated, len(records))
	for i, record := range records {
		r, at, err := validateStopCheck(record)
		if err != nil {
			return err
		}
		rows[i] = validated{r, at}
	}
	for start := 0; start < len(rows); start += stopCheckChunkSize {
		end := min(start+stopCheckChunkSize, len(rows))
		var query strings.Builder
		query.WriteString(`INSERT INTO stop_check_records (id, check_id, result, occurred_at, project, host) VALUES `)
		args := make([]any, 0, (end-start)*6)
		for i, row := range rows[start:end] {
			if i > 0 {
				query.WriteByte(',')
			}
			fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d,$%d)", i*6+1, i*6+2, i*6+3, i*6+4, i*6+5, i*6+6)
			args = append(args, row.record.ID, row.record.CheckID, row.record.Result, row.at, row.record.Project, row.record.Host)
		}
		query.WriteString(` ON CONFLICT (id) DO NOTHING`)
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return err
		}
	}
	return nil
}
