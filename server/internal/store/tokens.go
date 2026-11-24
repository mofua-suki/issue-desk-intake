package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"issue-desk-intake/server/internal/auth"
)

type PCTokenRecord struct {
	ID       string
	Name     string
	Hash     string
	IssuedAt time.Time
}

var ErrInvalidPCName = errors.New("invalid PC name")
var pcNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func (s *SQLite) AddPCToken(ctx context.Context, token PCTokenRecord) (bool, error) {
	if !pcNamePattern.MatchString(token.Name) {
		return false, ErrInvalidPCName
	}
	result, err := s.db.ExecContext(ctx, "INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING", token.ID, token.Name, token.Hash, token.IssuedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *SQLite) PCToken(ctx context.Context, hash string) (auth.Token, bool, error) {
	return s.tokenWhere(ctx, `SELECT t.id,t.name,CASE WHEN g.token_id IS NULL THEN 0 ELSE 1 END
FROM api_tokens t LEFT JOIN api_token_grants g ON t.id=g.token_id WHERE t.hash=?`, hash)
}

// tokenWhere reads one token with its delegation flag. The query selects id, name and the flag.
func (s *SQLite) tokenWhere(ctx context.Context, query string, arg string) (auth.Token, bool, error) {
	var token auth.Token
	var delegated int
	err := s.db.QueryRowContext(ctx, query, arg).Scan(&token.ID, &token.Name, &delegated)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Token{}, false, nil
	}
	if err != nil {
		return auth.Token{}, false, err
	}
	token.Delegated = delegated == 1
	return token, true, nil
}

func (s *SQLite) TouchPCToken(ctx context.Context, id string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE api_tokens SET last_used_at=? WHERE id=?", at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("PC token was revoked")
	}
	return nil
}

func (s *SQLite) RevokePCToken(ctx context.Context, name string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE name=?", name)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *SQLite) SetDelegated(ctx context.Context, id string, delegated bool, at time.Time) (bool, error) {
	if !delegated {
		result, err := s.db.ExecContext(ctx, "DELETE FROM api_token_grants WHERE token_id=?", id)
		if err != nil {
			return false, err
		}
		count, err := result.RowsAffected()
		return count == 1, err
	}
	query, err := s.db.dialect.InsertIgnore("api_token_grants", []string{"token_id", "granted_at"}, "SELECT id,? FROM api_tokens WHERE id=?")
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, query, at.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *SQLite) AddCLIKey(ctx context.Context, hash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO cli_keys(hash,expires) VALUES(?,?) ON CONFLICT(hash) DO UPDATE SET expires=excluded.expires", hash, expires.UnixMilli())
	return err
}

func (s *SQLite) ConsumeCLIKey(ctx context.Context, hash string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM cli_keys WHERE hash=? AND expires>=?", hash, now.UnixMilli())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// AuthStore joins SQLite screen/PC credentials with PostgreSQL hostd credentials.
type AuthStore struct {
	*SQLite
	Postgres *sql.DB
}

var _ auth.Store = (*AuthStore)(nil)

func (s *AuthStore) HostdToken(ctx context.Context, hash string) (auth.Token, bool, error) {
	if s.Postgres == nil {
		return auth.Token{}, false, errors.New("hostd token database is unavailable")
	}
	var token auth.Token
	err := s.Postgres.QueryRowContext(ctx, "SELECT host FROM host_image_tokens WHERE hash=$1", hash).Scan(&token.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Token{}, false, nil
	}
	return token, err == nil, err
}
