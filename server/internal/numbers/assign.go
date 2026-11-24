package numbers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

type Candidate struct {
	Number      int64
	CreatedAt   string
	PullRequest bool
}
type List struct {
	Project string
	Issues  []Candidate
}

func (s *Store) Assign(ctx context.Context, lists []List, projects []string, complete bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		if _, err := s.reconcile(ctx); err != nil {
			return false, err
		}
	}
	s.checked = false
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var next int64
	err = tx.QueryRowContext(ctx, bindSQL("SELECT next FROM desk_counter WHERE id=1")).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	savedNext := next
	if next == 0 && !readyToNumber(lists, projects, complete) {
		return false, nil
	}
	var top sql.NullInt64
	if err = tx.QueryRowContext(ctx, bindSQL("SELECT max(number) FROM desk_numbers")).Scan(&top); err != nil {
		return false, err
	}
	next = max(next, int64(1), top.Int64+1)
	knownProjects, err := readKnownProjects(ctx, tx)
	if err != nil {
		return false, err
	}
	now := iso(s.Now())
	candidates, next, newProjects, err := takeCandidates(ctx, tx, lists, knownProjects, complete, next, now)
	if err != nil {
		return false, err
	}
	existing, err := readNumbered(ctx, tx, candidates)
	if err != nil {
		return false, err
	}
	issued, next, err := issueNumbers(ctx, tx, candidates, existing, next, now)
	if err != nil {
		return false, err
	}
	if len(issued) == 0 && !newProjects && next == savedNext {
		if err = tx.Commit(); err != nil {
			return false, err
		}
		s.checked = true
		return true, nil
	}
	if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_counter(id,next) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET next=excluded.next"), next); err != nil {
		return false, err
	}
	if err = s.append(issued); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.checked = true
	return true, nil
}

// readyToNumber tells whether the first numbering may start: every project
// must have been listed completely.
func readyToNumber(lists []List, projects []string, complete bool) bool {
	if !complete {
		return false
	}
	for _, project := range projects {
		found := false
		for _, list := range lists {
			if list.Project == project {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// readKnownProjects reads the projects whose issues have been numbered before.
func readKnownProjects(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	var err error
	knownProjects := map[string]bool{}
	rows, err := tx.QueryContext(ctx, bindSQL("SELECT project FROM desk_projects"))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var project string
		if err = rows.Scan(&project); err != nil {
			rows.Close()
			return nil, err
		}
		knownProjects[project] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return knownProjects, nil
}

type numberCandidate struct {
	Project string
	Candidate
}

// takeCandidates selects the issues to number in creation order and registers
// the projects seen for the first time. The first listing of a new project
// keeps its existing issue numbers clear of the counter.
func takeCandidates(ctx context.Context, tx *sql.Tx, lists []List, knownProjects map[string]bool, complete bool, next int64, now string) ([]numberCandidate, int64, bool, error) {
	var err error
	candidates := []numberCandidate{}
	newProjects := false
	for _, list := range lists {
		known := knownProjects[list.Project]
		if !known && !complete {
			continue
		}
		for _, issue := range list.Issues {
			if issue.PullRequest || !valid(issue.Number) {
				continue
			}
			if !known {
				if issue.Number >= largest {
					return nil, 0, false, errors.New("共通番号が発行上限に達しています")
				}
				next = max(next, issue.Number+1)
			}
			candidates = append(candidates, numberCandidate{list.Project, issue})
		}
		if !known {
			if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_projects(project,scanned_at) VALUES(?,?)"), list.Project, now); err != nil {
				return nil, 0, false, err
			}
			knownProjects[list.Project] = true
			newProjects = true
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Number < b.Number
	})
	return candidates, next, newProjects, nil
}

// readNumbered reads which candidates already have a number.
func readNumbered(ctx context.Context, tx *sql.Tx, candidates []numberCandidate) (map[Issue]bool, error) {
	existing := map[Issue]bool{}
	if len(candidates) > 0 {
		wanted := make([]Issue, 0, len(candidates))
		for _, c := range candidates {
			wanted = append(wanted, Issue{c.Project, c.Number})
		}
		data, err := json.Marshal(wanted)
		if err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, bindSQL(`SELECT project,gitea FROM desk_numbers WHERE EXISTS (SELECT 1 FROM jsonb_to_recordset(?::jsonb) AS wanted(project text, number bigint) WHERE desk_numbers.project=wanted.project AND desk_numbers.gitea=wanted.number)`), string(data))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var issue Issue
			if err = rows.Scan(&issue.Project, &issue.Number); err != nil {
				rows.Close()
				return nil, err
			}
			existing[issue] = true
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// issueNumbers gives the next numbers to the candidates without one.
func issueNumbers(ctx context.Context, tx *sql.Tx, candidates []numberCandidate, existing map[Issue]bool, next int64, now string) ([]Entry, int64, error) {
	var err error
	issued := []Entry{}
	for _, c := range candidates {
		key := Issue{c.Project, c.Number}
		if existing[key] {
			continue
		}
		existing[key] = true
		if next >= largest {
			return nil, 0, errors.New("共通番号が発行上限に達しています")
		}
		if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(?,?,?,?)"), next, c.Project, c.Number, now); err != nil {
			return nil, 0, err
		}
		issued = append(issued, Entry{Number: next, Project: c.Project, Gitea: c.Number, At: now})
		next++
	}
	return issued, next, nil
}
func (s *Store) Rename(ctx context.Context, from, to string) error {
	if from == "" || to == "" || from == to {
		return errors.New("共通番号の改名対象が不正です")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		if _, err := s.reconcile(ctx); err != nil {
			return err
		}
	}
	s.checked = false
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var source, target int
	if err = tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM desk_numbers WHERE project=?"), from).Scan(&source); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM desk_numbers WHERE project=?"), to).Scan(&target); err != nil {
		return err
	}
	if source > 0 && target > 0 {
		return errors.New("移行先の共通番号が既にあります")
	}
	if source > 0 {
		if err = s.append([]Entry{{Rename: []string{from, to}, At: iso(s.Now())}}); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, bindSQL("UPDATE desk_numbers SET project=? WHERE project=?"), to, from); err != nil {
		return err
	}
	var scanned int
	if err = tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM desk_projects WHERE project=?"), from).Scan(&scanned); err != nil {
		return err
	}
	if scanned > 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM desk_projects WHERE project=?", to); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, bindSQL("UPDATE desk_projects SET project=? WHERE project=?"), to, from); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.checked = true
	return nil
}
