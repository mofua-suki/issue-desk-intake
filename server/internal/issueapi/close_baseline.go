package issueapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
)

const closeBaselineKey = "closeReasonBaseline"

type listBaselineKey struct{}
type listBaselineValue struct{ baseline Object }

func (s *Service) closeBaseline(ctx context.Context) (Object, error) {
	if value, ok := ctx.Value(listBaselineKey{}).(listBaselineValue); ok {
		return value.baseline, nil
	}

	var raw string
	e := s.o.AppDB.QueryRowContext(ctx, bindSQL(`SELECT json FROM settings WHERE id=?`), closeBaselineKey).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	baseline := Object{}
	if e = json.Unmarshal([]byte(raw), &baseline); e != nil {
		return nil, e
	}
	return baseline, nil
}
func (s *Service) recordCloseBaseline(ctx context.Context, closed []Object) error {
	issues := Object{}
	for _, issue := range closed {
		if issue["state"] == "closed" {
			issues[fmt.Sprintf("%s#%d", fleet.Text(issue["project"]), int64Number(issue["number"]))] = issue["closedAt"]
		}
	}
	raw, e := json.Marshal(Object{"recordedAt": s.now(), "source": "user_specified_legacy", "reason": "other_route", "issues": issues})
	if e != nil {
		return e
	}
	_, e = s.o.AppDB.ExecContext(ctx, bindSQL(`INSERT INTO settings(id,json)VALUES(?,?)ON CONFLICT(id)DO NOTHING`), closeBaselineKey, string(raw))
	return e
}
func int64Number(v any) int64 { n, _ := number(v); return n }
func (s *Service) ensureCloseBaseline(ctx context.Context) error {
	if baseline, e := s.closeBaseline(ctx); e != nil || baseline != nil {
		return e
	}
	closed := []Object{}
	for _, repo := range s.repositories() {
		issues, e := s.o.Gitea.Issues(ctx, repo, "all")
		if e != nil {
			return e
		}
		for _, issue := range issues {
			allowed, e := s.requestAuthorChecked(issue)
			if e != nil {
				return e
			}
			if issue["pull_request"] == nil && allowed && issue["state"] == "closed" {
				closed = append(closed, Object{"project": repo, "number": issue["number"], "state": "closed", "closedAt": issue["closed_at"]})
			}
		}
	}
	return s.recordCloseBaseline(ctx, closed)
}
func (s *Service) closeReason(ctx context.Context, repo string, issue Object) (Object, error) {
	key := fmt.Sprintf("%s#%d", repo, int64Number(issue["number"]))
	manual, e := s.readListRecord(ctx, "close_reasons", key)
	if e != nil {
		return nil, e
	}
	closed := issue["closed_at"]
	if manual != nil && manual["closedAt"] == closed {
		source := "manual"
		if manual["source"] == "token" {
			source = "token"
		}
		result := Object{"reason": manual["reason"], "source": source}
		if manual["route"] != nil {
			result["route"] = manual["route"]
		}
		return result, nil
	}
	baseline, e := s.closeBaseline(ctx)
	if e != nil {
		return nil, e
	}
	if at, has := fleet.ObjectOrEmpty(baseline["issues"])[key]; has && at == closed {
		return Object{"reason": baseline["reason"], "source": "legacy", "recordedAt": baseline["recordedAt"]}, nil
	}
	source := "none"
	if baseline == nil {
		source = "unconfirmed"
	}
	return Object{"reason": "unknown", "source": source}, nil
}
func (s *Service) forgetCloseReason(ctx context.Context, repo string, n int64) error {
	key := fmt.Sprintf("%s#%d", repo, n)
	tx, e := s.o.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `DELETE FROM close_reasons WHERE id=?`, key); e != nil {
		return e
	}
	var raw string
	e = tx.QueryRowContext(ctx, bindSQL(`SELECT json FROM settings WHERE id=?`), closeBaselineKey).Scan(&raw)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil {
		baseline := Object{}
		if e = json.Unmarshal([]byte(raw), &baseline); e != nil {
			return e
		}
		issues := fleet.ObjectOrEmpty(baseline["issues"])
		if _, has := issues[key]; has {
			delete(issues, key)
			baseline["issues"] = issues
			encoded, e := json.Marshal(baseline)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, bindSQL(`UPDATE settings SET json=? WHERE id=?`), string(encoded), closeBaselineKey); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
