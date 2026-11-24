package numbers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

func (s *Store) Reconcile(ctx context.Context) (Check, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcile(ctx)
}
func (s *Store) reconcile(ctx context.Context) (result Check, err error) {
	s.checked = false
	result.Restored = []int64{}
	result.Conflicts = []int64{}
	s.checked = false
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	entries, invalid, err := s.read()
	if err != nil {
		return result, err
	}
	result.Invalid = invalid
	wanted, highest := replayNumberLog(entries)
	missing, highest, err := readUnlogged(ctx, tx, wanted, highest)
	if err != nil {
		return result, err
	}
	if err = s.restoreNumbers(ctx, tx, wanted, &result); err != nil {
		return result, err
	}
	next, err := advanceCounter(ctx, tx, highest)
	if err != nil {
		return result, err
	}
	if next > 0 {
		result.Next = &next
	}
	if err = s.append(missing); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result.Recorded = len(missing)
	sort.Slice(result.Restored, func(i, j int) bool { return result.Restored[i] < result.Restored[j] })
	sort.Slice(result.Conflicts, func(i, j int) bool { return result.Conflicts[i] < result.Conflicts[j] })
	s.checked = true
	return result, nil
}

// replayNumberLog replays the number log from the newest entry, following
// renames, and returns the latest entry of each number and issue with the
// highest number used.
func replayNumberLog(entries []Entry) ([]Entry, int64) {
	aliases := map[string]string{}
	resolve := func(p string) string {
		if a, ok := aliases[p]; ok {
			return a
		}
		return p
	}
	seenNumbers := map[int64]bool{}
	seenIssues := map[string]bool{}
	wanted := []Entry{}
	highest := int64(0)
	for index := len(entries) - 1; index >= 0; index-- {
		e := entries[index]
		if len(e.Rename) == 2 {
			aliases[e.Rename[0]] = resolve(e.Rename[1])
			continue
		}
		if e.Next > 0 {
			highest = max(highest, e.Next-1)
			continue
		}
		highest = max(highest, e.Number)
		e.Project = resolve(e.Project)
		key := fmt.Sprintf("%s#%d", e.Project, e.Gitea)
		if seenNumbers[e.Number] || seenIssues[key] {
			continue
		}
		seenNumbers[e.Number] = true
		seenIssues[key] = true
		wanted = append(wanted, e)
	}
	return wanted, highest
}

// readUnlogged reads the stored numbers that the log does not record.
func readUnlogged(ctx context.Context, tx *sql.Tx, wanted []Entry, highest int64) ([]Entry, int64, error) {
	recorded := map[string]bool{}
	for _, e := range wanted {
		recorded[fmt.Sprintf("%d %s#%d", e.Number, e.Project, e.Gitea)] = true
	}
	rows, err := tx.QueryContext(ctx, bindSQL("SELECT number,project,gitea,assigned_at FROM desk_numbers ORDER BY number"))
	if err != nil {
		return nil, 0, err
	}
	missing := []Entry{}
	for rows.Next() {
		var e Entry
		if err = rows.Scan(&e.Number, &e.Project, &e.Gitea, &e.At); err != nil {
			rows.Close()
			return nil, 0, err
		}
		highest = max(highest, e.Number)
		if !recorded[fmt.Sprintf("%d %s#%d", e.Number, e.Project, e.Gitea)] {
			missing = append(missing, e)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()
	return missing, highest, nil
}

// restoreNumbers stores the logged numbers missing from the store, and records
// those that conflict with a stored number.
func (s *Store) restoreNumbers(ctx context.Context, tx *sql.Tx, wanted []Entry, result *Check) error {
	var err error
	for index := len(wanted) - 1; index >= 0; index-- {
		e := wanted[index]
		var project string
		var gitea int64
		err = tx.QueryRowContext(ctx, bindSQL("SELECT project,gitea FROM desk_numbers WHERE number=?"), e.Number).Scan(&project, &gitea)
		if err == nil {
			if project != e.Project || gitea != e.Gitea {
				result.Conflicts = append(result.Conflicts, e.Number)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM desk_numbers WHERE project=? AND gitea=?"), e.Project, e.Gitea).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			result.Conflicts = append(result.Conflicts, e.Number)
			continue
		}
		if e.At == "" {
			e.At = iso(s.Now())
		}
		if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(?,?,?,?)"), e.Number, e.Project, e.Gitea, e.At); err != nil {
			return err
		}
		result.Restored = append(result.Restored, e.Number)
	}
	return nil
}

// advanceCounter moves the counter past the highest number used.
func advanceCounter(ctx context.Context, tx *sql.Tx, highest int64) (int64, error) {
	var next int64
	err := tx.QueryRowContext(ctx, bindSQL("SELECT next FROM desk_counter WHERE id=1")).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if highest >= largest {
		return 0, errors.New("共通番号が発行上限に達しています")
	}
	if highest > 0 && next <= highest {
		next = highest + 1
		if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_counter(id,next) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET next=excluded.next"), next); err != nil {
			return 0, err
		}
	}
	return next, nil
}
func (s *Store) Reserve(ctx context.Context, next int64) (int64, error) {
	if next < 2 || !valid(next) {
		return 0, errors.New("次の共通番号は2以上の整数で指定してください")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.checked {
		if _, err := s.reconcile(ctx); err != nil {
			return 0, err
		}
	}
	s.checked = false
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var current int64
	err = tx.QueryRowContext(ctx, bindSQL("SELECT next FROM desk_counter WHERE id=1")).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	next = max(next, current)
	if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO desk_counter(id,next) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET next=excluded.next"), next); err != nil {
		return 0, err
	}
	if err = s.append([]Entry{{Next: next, At: iso(s.Now())}}); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	s.checked = true
	return next, nil
}
