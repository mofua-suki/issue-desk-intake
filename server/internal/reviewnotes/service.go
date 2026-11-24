package reviewnotes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Classifier func(context.Context, []Note, []Item) ([]Classification, error)

type Service struct {
	DB       *sql.DB
	Now      func() time.Time
	Classify Classifier
}

var japan = time.FixedZone("JST", 9*60*60)

// Run claims the repository's JST date in an autocommitted statement BEFORE
// loading notes or calling the classifier. Its boolean reports a claimed attempt,
// including one that failed later. Failure never advances notes/cursor.
func (s *Service) Run(ctx context.Context, repo string, config Config) (bool, error) {
	if err := config.validate(); err != nil {
		return false, err
	}
	if s.DB == nil || strings.TrimSpace(repo) == "" {
		return false, errors.New("注意点の保存先とリポジトリが必要です")
	}
	// Determine the date after waiting for a pool connection, including midnight.
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	date := now.In(japan).Format("2006-01-02")
	var raw []byte
	err = conn.QueryRowContext(ctx, `INSERT INTO repo_note_processing(repo,attempt_date) VALUES($1,$2)
ON CONFLICT(repo) DO UPDATE SET attempt_date=EXCLUDED.attempt_date
WHERE repo_note_processing.attempt_date IS NULL OR repo_note_processing.attempt_date < EXCLUDED.attempt_date
RETURNING notes`, repo, date).Scan(&raw)
	conn.Close()
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	previous, err := Decode(raw)
	if err != nil {
		return true, err
	}
	notes, cursor, err := s.read(ctx, repo, previous.Cursor, now.Add(-time.Duration(config.StaleDays)*24*time.Hour))
	if err != nil {
		return true, err
	}
	classified, cache, err := s.classify(ctx, repo, previous, notes, now, config)
	if err != nil {
		return true, err
	}
	next, err := Apply(previous, notes, classified, now, config)
	if err != nil {
		return true, err
	}
	next.Cursor = cursor
	next.Classifications = cache
	raw, err = json.Marshal(next)
	if err != nil {
		return true, err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE repo_note_processing SET notes=$3::jsonb WHERE repo=$1 AND attempt_date=$2`, repo, date, string(raw))
	if err != nil {
		return true, err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed != 1 {
		err = errors.New("注意点の試行日が変わったため保存しませんでした")
	}
	return true, err
}

func (s *Service) read(ctx context.Context, repo string, cursor int64, cutoff time.Time) ([]Note, int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,kind,at,data FROM repo_notes
WHERE repo=$1 AND id>$2 AND kind IN ('finding','followup') ORDER BY id ASC LIMIT 200`, repo, cursor)
	if err != nil {
		return nil, cursor, err
	}
	defer rows.Close()
	notes := []Note{}
	for rows.Next() {
		var note Note
		var raw []byte
		if err := rows.Scan(&note.ID, &note.SourceKind, &note.At, &raw); err != nil {
			return nil, cursor, err
		}
		cursor = note.ID // Invalid/old rows are consumed too, avoiding a stuck batch.
		var data struct {
			Issue int64  `json:"issue"`
			Text  string `json:"text"`
		}
		if note.At.Before(cutoff) || json.Unmarshal(raw, &data) != nil || data.Issue < 1 || strings.TrimSpace(data.Text) == "" {
			continue
		}
		note.Issue, note.Text = data.Issue, data.Text
		notes = append(notes, note)
	}
	return notes, cursor, rows.Err()
}
