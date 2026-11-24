package consultations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log"
	"sync"
	"time"

	"issue-desk-intake/server/internal/contract"
)

// Record retains fields not owned by the current operation. Each write validates
// its own fields without dropping additional conversation and decision data.
type Record map[string]any

// SetDraftError keeps the public error and its optional code in sync.
func SetDraftError(d Record, message, code any) {
	d["error"], d["errorCode"] = message, code
}

type Records struct {
	DB           *sql.DB
	draftOnce    sync.Once
	draftGate    chan struct{}
	draftCache   map[DraftReadPurpose]map[string]cachedDraft
	draftMetrics map[DraftReadPurpose]draftCacheMetric
}

var tables = map[string]bool{"drafts": true, "todos": true, "actions": true, "close_requests": true, "close_reasons": true, "projects": true, "proposals": true, "proposal_scans": true, "decisions": true}

// Keep exceptional legacy records in the recovery read so Go can validate
// and report them. PostgreSQL extraction rejects escaped NUL and lone
// surrogate halves. Let Go check any record with a surrogate escape.
const recoverableReceiptSQL = `CASE
	WHEN position('\u0000' in json) > 0 OR position('\ud' in json) > 0 OR position('\uD' in json) > 0 THEN true
	WHEN json IS NOT JSON OBJECT THEN true
	ELSE (json::json->>'phase') IN ('accepted','processing') END`

func OpenRecords(ctx context.Context, db *sql.DB) (*Records, error) {
	if db == nil {
		return nil, errors.New("consultations require an application database")
	}
	for name := range tables {
		if _, err := db.ExecContext(ctx, bindSQL("CREATE TABLE IF NOT EXISTS "+name+"(id TEXT PRIMARY KEY,json TEXT NOT NULL)")); err != nil {
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS images(id TEXT PRIMARY KEY,scope TEXT NOT NULL,message TEXT NOT NULL,json TEXT NOT NULL,bytes `+binaryType()+` NOT NULL,bound INTEGER NOT NULL DEFAULT 0); CREATE INDEX IF NOT EXISTS images_scope ON images(scope); CREATE TABLE IF NOT EXISTS intake_receipts(id TEXT PRIMARY KEY,sender TEXT NOT NULL,send_id TEXT NOT NULL,json TEXT NOT NULL,UNIQUE(sender,send_id));`)); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, bindSQL("CREATE INDEX IF NOT EXISTS intake_receipts_recovery ON intake_receipts(id) WHERE "+recoverableReceiptSQL)); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS draft_withdrawals(sequence INTEGER PRIMARY KEY,draft TEXT NOT NULL,project TEXT,title TEXT,operation TEXT NOT NULL,at TEXT NOT NULL,created_at TEXT,via TEXT NOT NULL,token_name TEXT,reason TEXT,parent TEXT);CREATE INDEX IF NOT EXISTS draft_withdrawals_at ON draft_withdrawals(at);`)); err != nil {
		return nil, err
	}
	// Prepare proxy tables and indexes before request transactions take write
	// locks: concurrent index creation and inserts can deadlock.
	if err := proxyTables(ctx, db); err != nil {
		return nil, err
	}
	return &Records{DB: db}, nil
}

func decode(raw string) (Record, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if record == nil {
		return nil, errors.New("saved record is not an object")
	}
	return record, nil
}

func (s *Records) Get(ctx context.Context, table, id string) (Record, bool, error) {
	if !tables[table] {
		return nil, false, errors.New("invalid record collection")
	}
	var raw string
	err := s.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM "+table+" WHERE id=?"), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	record, err := decode(raw)
	return record, err == nil, err
}

func (s *Records) List(ctx context.Context, table string) ([]Record, error) {
	if !tables[table] {
		return nil, errors.New("invalid record collection")
	}
	rows, err := s.DB.QueryContext(ctx, bindSQL("SELECT json FROM "+table+" ORDER BY "+rowOrder(true)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Record, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ListTodoEntries retains database keys independently of the JSON id. Invalid
// PostgreSQL JSON is retried as text because decode accepts some such inputs.
func (s *Records) ListTodoEntries(ctx context.Context) ([]DraftFields, error) {
	entries, err := s.readDraftEntries(ctx, todoProjectionSQL())
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "22P02" || state.SQLState() == "22P05") {
		log.Print("consultations: todo projection JSON conversion failed; using full read")
		return s.readDraftEntries(ctx, "SELECT id, json FROM todos ORDER BY "+rowOrder(true))
	}
	return entries, err
}

// GetTodos fetches a selected page in one query, omitting disappeared rows.
func (s *Records) GetTodos(ctx context.Context, ids []string) ([]Record, error) {
	result := make([]Record, 0, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	query, args := selectedTodosSQL(ids)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// DraftReadPurpose identifies a projection for a particular consumer. These
// records are read-only and must never be passed to Change as replacements.
// Projection may change key presence and normalize numbers in nested fields.
type DraftReadPurpose string

// DraftFields keeps the database key separate from the possibly missing or
// malformed id in the stored JSON. Use Key when loading a selected page.
type DraftFields struct {
	Key    string
	Record Record
}

const (
	DraftReadPage         DraftReadPurpose = "page"
	DraftReadAlerts       DraftReadPurpose = "alerts"
	DraftReadScreen       DraftReadPurpose = "screen"
	DraftReadRecovery     DraftReadPurpose = "recovery"
	DraftReadFiling       DraftReadPurpose = "filing"
	DraftReadFilingAlerts DraftReadPurpose = "filing_alerts"
)

// ListDraftFields reads only the fields needed to select a page or produce
// alerts, or a screen summary. Full page items must subsequently be loaded with GetDrafts.
func (s *Records) ListDraftFields(ctx context.Context, purpose DraftReadPurpose) ([]Record, error) {
	entries, err := s.ListDraftEntries(ctx, purpose)
	if err != nil {
		return nil, err
	}
	result := make([]Record, len(entries))
	for i, entry := range entries {
		result[i] = entry.Record
	}
	return result, nil
}

// ListDraftEntries provides the database key needed to retrieve selected rows.
func (s *Records) ListDraftEntries(ctx context.Context, purpose DraftReadPurpose) ([]DraftFields, error) {
	query, err := draftProjectionSQL(purpose)
	if err != nil {
		return nil, err
	}
	result, err := s.readCachedDraftEntries(ctx, purpose, query)
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "22P02" || state.SQLState() == "22P05") {
		// PostgreSQL rejects some JSON accepted by decode. Retry the entire
		// invocation with the same full JSON and ordering as List, retaining keys.
		log.Print("consultations: draft projection JSON conversion failed; using full read")
		return s.readDraftEntries(ctx, "SELECT id, json FROM drafts ORDER BY "+rowOrder(true))
	}
	return result, err
}

func (s *Records) readDraftEntries(ctx context.Context, query string) ([]DraftFields, error) {
	rows, err := s.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]DraftFields, 0)
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, DraftFields{Key: id, Record: r})
	}
	return result, rows.Err()
}

// GetDrafts loads full records for selected database IDs in List order.
// Missing IDs are omitted; an empty selection performs no database query.
func (s *Records) GetDrafts(ctx context.Context, ids []string) ([]Record, error) {
	entries, err := s.selectedDraftEntries(ctx, ids, false)
	if entries == nil {
		return nil, err
	}
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Record)
	}
	return result, err
}

// GetDraftEntries loads selected full records with their database keys.
// Missing rows are omitted and an empty selection does not query the database.
func (s *Records) GetDraftEntries(ctx context.Context, ids []string) ([]DraftFields, error) {
	return s.selectedDraftEntries(ctx, ids, true)
}

func (s *Records) selectedDraftEntries(ctx context.Context, ids []string, withKey bool) ([]DraftFields, error) {
	result := make([]DraftFields, 0)
	if len(ids) == 0 {
		return result, nil
	}
	query, args := selectedDraftsSQL(ids)
	if withKey {
		query = "SELECT id, " + query[len("SELECT "):]
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if withKey {
			err = rows.Scan(&key, &raw)
		} else {
			err = rows.Scan(&raw)
		}
		if err != nil {
			return nil, err
		}
		record, err := decode(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, DraftFields{Key: key, Record: record})
	}
	return result, rows.Err()
}

type Change func(*sql.Tx, Record) (Record, error)

func (s *Records) Change(ctx context.Context, table, id string, change Change) (Record, error) {
	if !tables[table] {
		return nil, errors.New("invalid record collection")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	var old Record
	err = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM "+table+" WHERE id=?"), id).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	found := err == nil
	if found {
		old, err = decode(raw)
		if err != nil {
			return nil, err
		}
	}
	next, err := change(tx, old)
	if err != nil {
		return nil, err
	}
	if next == nil {
		if found {
			if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id=? AND json=?", id, raw); err != nil {
				return nil, err
			}
		}
	} else {
		encoded, err := json.Marshal(next)
		if err != nil {
			return nil, err
		}
		if found {
			r, err := tx.ExecContext(ctx, bindSQL("UPDATE "+table+" SET json=? WHERE id=? AND json=?"), string(encoded), id, raw)
			if err != nil {
				return nil, err
			}
			count, err := r.RowsAffected()
			if err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, refuse(409, "別の操作で内容が更新されています。")
			}
		} else {
			if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO "+table+"(id,json) VALUES(?,?)"), id, string(encoded)); err != nil {
				return nil, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

func refuse(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}

// text stays a one-line name: the guarded DELETE statements in todos.go and
// split-family.go read ids through it.
func text(value any) string { return fleet.Text(value) }
func integer(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true
		}
	}
	return 0, false
}
func timestamp(now time.Time) string { return contract.ISO(now) }
func clone(value Record) Record {
	out := make(Record, len(value))
	for k, v := range value {
		out[k] = v
	}
	return out
}
func problem(err error) *contract.Problem {
	var p *contract.Problem
	if errors.As(err, &p) {
		return p
	}
	contract.LogFailure("相談の保存", err)
	return refuse(503, "保存した内容を読み書きできません。")
}
