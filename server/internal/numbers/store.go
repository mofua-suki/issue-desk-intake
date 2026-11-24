// Package numbers preserves the shared issue identifiers and their append-only
// recovery journal independently of SQLite backup age.
package numbers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"sync"
	"time"
)

type Store struct {
	DB      *sql.DB
	Journal string
	Now     func() time.Time
	mu      sync.Mutex
	checked bool
}
type Issue struct {
	Project string `json:"project"`
	Number  int64  `json:"number"`
}
type Check struct {
	Restored  []int64 `json:"restored"`
	Conflicts []int64 `json:"conflicts"`
	Recorded  int     `json:"recorded"`
	Invalid   int     `json:"invalid"`
	Next      *int64  `json:"next"`
}
type Entry struct {
	Number  int64    `json:"number,omitempty"`
	Project string   `json:"project,omitempty"`
	Gitea   int64    `json:"gitea,omitempty"`
	At      string   `json:"at,omitempty"`
	Rename  []string `json:"rename,omitempty"`
	Next    int64    `json:"next,omitempty"`
}

const largest = int64(9007199254740991)

func valid(n int64) bool     { return n > 0 && n <= largest }
func iso(t time.Time) string { return contract.ISO(t) }

// OpenLedger uses the supplied journal path without changing it.
func OpenLedger(ctx context.Context, db *sql.DB, journalPath string) (*Store, error) {
	if db == nil || journalPath == "" {
		return nil, errors.New("共通番号の保存先が必要です")
	}
	_, err := db.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS desk_numbers(number INTEGER PRIMARY KEY CHECK(number > 0), project TEXT NOT NULL,gitea INTEGER NOT NULL CHECK(gitea > 0),assigned_at TEXT NOT NULL,UNIQUE(project,gitea));CREATE TABLE IF NOT EXISTS desk_projects(project TEXT PRIMARY KEY,scanned_at TEXT NOT NULL);CREATE TABLE IF NOT EXISTS desk_counter(id INTEGER PRIMARY KEY CHECK(id=1),next INTEGER NOT NULL CHECK(next>0));`))
	if err != nil {
		return nil, err
	}
	return &Store{DB: db, Journal: journalPath, Now: time.Now}, nil
}
func (s *Store) Number(ctx context.Context, project string, gitea int64) (int64, bool, error) {
	var number int64
	err := s.DB.QueryRowContext(ctx, bindSQL("SELECT number FROM desk_numbers WHERE project=? AND gitea=?"), project, gitea).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return number, err == nil, err
}
func (s *Store) Issue(ctx context.Context, number int64) (Issue, bool, error) {
	var issue Issue
	err := s.DB.QueryRowContext(ctx, bindSQL("SELECT project,gitea FROM desk_numbers WHERE number=?"), number).Scan(&issue.Project, &issue.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, false, nil
	}
	return issue, err == nil, err
}
func (s *Store) Ready(ctx context.Context, projects []string) (bool, error) {
	var next int64
	err := s.DB.QueryRowContext(ctx, bindSQL("SELECT next FROM desk_counter WHERE id=1")).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, project := range projects {
		var n int
		if err := s.DB.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM desk_projects WHERE project=?"), project).Scan(&n); err != nil {
			return false, err
		}
		if n != 1 {
			return false, nil
		}
	}
	return true, nil
}

// All returns a snapshot of assigned issue numbers for list projections.
func (s *Store) All(ctx context.Context) (map[Issue]int64, error) {
	rows, err := s.DB.QueryContext(ctx, bindSQL("SELECT project,gitea,number FROM desk_numbers"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[Issue]int64{}
	for rows.Next() {
		var issue Issue
		var n int64
		if err := rows.Scan(&issue.Project, &issue.Number, &n); err != nil {
			return nil, err
		}
		result[issue] = n
	}
	return result, rows.Err()
}

// ForIssues returns assigned numbers for the exact project and issue pairs.
func (s *Store) ForIssues(ctx context.Context, issues []Issue) (map[Issue]int64, error) {
	result := map[Issue]int64{}
	if len(issues) == 0 {
		return result, nil
	}
	data, err := json.Marshal(issues)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, bindSQL(`SELECT project,gitea,number FROM desk_numbers WHERE EXISTS (SELECT 1 FROM jsonb_to_recordset(?::jsonb) AS wanted(project text, number bigint) WHERE desk_numbers.project=wanted.project AND desk_numbers.gitea=wanted.number)`), string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var issue Issue
		var number int64
		if err := rows.Scan(&issue.Project, &issue.Number, &number); err != nil {
			return nil, err
		}
		result[issue] = number
	}
	return result, rows.Err()
}
