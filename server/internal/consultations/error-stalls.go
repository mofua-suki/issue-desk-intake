package consultations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ErrorStalls struct {
	service           *Service
	revision, runtime string
	mu                sync.Mutex
	checking          bool
	outcomes          map[string]Record
}

func NewErrorStalls(ctx context.Context, s *Service, revision, runtime string) (*ErrorStalls, error) {
	if s == nil || !filepath.IsAbs(runtime) {
		return nil, errors.New("相談の失敗記録の保存先が不正です")
	}
	_, err := s.options.Records.DB.ExecContext(ctx, bindSQL(`CREATE TABLE IF NOT EXISTS intake_error_failures(id `+identityType()+`,draft TEXT NOT NULL,gist TEXT NOT NULL,at TEXT NOT NULL,json TEXT NOT NULL);CREATE TABLE IF NOT EXISTS intake_error_stalls(id TEXT PRIMARY KEY,json TEXT NOT NULL);`))
	if err != nil {
		return nil, err
	}
	return &ErrorStalls{service: s, revision: revision, runtime: runtime, outcomes: map[string]Record{}}, nil
}

var gistUUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|\b[0-9a-f]{7,}\b|[0-9]+`)

func failureGist(d Record, err error) string {
	return prefix(strings.Join(strings.Fields(gistUUID.ReplaceAllString(MaskCredentials(scrubConversation(d, err.Error())), "#")), " "), 200)
}
func excluded(err error) bool {
	var failure *ai.Failure
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) || errors.As(err, &timeout) && timeout.Timeout()
}
func (s *ErrorStalls) rows(ctx context.Context, table string) ([]Record, error) {
	rows, err := s.service.options.Records.DB.QueryContext(ctx, bindSQL("SELECT json FROM "+table+" ORDER BY "+rowOrder(false)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		d, err := decode(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *ErrorStalls) save(ctx context.Context, d Record) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.service.options.Records.DB.ExecContext(ctx, bindSQL("INSERT INTO intake_error_stalls(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json"), d["id"], string(raw))
	return err
}
func (s *ErrorStalls) open(ctx context.Context) (Record, error) {
	rows, err := s.rows(ctx, "intake_error_stalls")
	if err != nil {
		return nil, err
	}
	for _, d := range rows {
		if d["resolvedAt"] == nil {
			return d, nil
		}
	}
	return nil, nil
}
func (s *ErrorStalls) gone(ctx context.Context, id string) (bool, error) {
	d, found, err := s.service.options.Records.Get(ctx, "drafts", id)
	if err != nil {
		return false, err
	}
	return !found || d["issue"] != nil || len(fleet.Items(d["issues"])) > 0 || is(text(d["status"]), "submitted", "project_handoff"), nil
}
func (s *ErrorStalls) resolve(ctx context.Context, d Record, result string) error {
	d["resolvedAt"], d["result"], d["resolvedRevision"] = timestamp(s.service.options.Now()), result, s.revision
	return s.save(ctx, d)
}
func (s *ErrorStalls) Failed(ctx context.Context, d Record, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if excluded(cause) {
		if s.checking {
			s.outcomes[text(d["id"])] = Record{"excluded": true}
		}
		return nil
	}
	f := Record{"draft": d["id"], "project": d["project"], "gist": failureGist(d, cause), "error": FailureCause(d, cause), "stage": FailureStage(cause), "at": timestamp(s.service.options.Now()), "lastRead": pendingRead(d["progress"]), "unread": unreadRecord(d["progress"])}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = s.service.options.Records.DB.ExecContext(ctx, bindSQL("INSERT INTO intake_error_failures(draft,gist,at,json) VALUES(?,?,?,?)"), f["draft"], f["gist"], f["at"], string(raw))
	if err != nil {
		return err
	}
	if s.checking {
		s.outcomes[text(d["id"])] = f
		return nil
	}
	return s.tick(ctx)
}
func (s *ErrorStalls) Succeeded(ctx context.Context, d Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.rows(ctx, "intake_error_failures")
	if err != nil {
		return err
	}
	gists := map[string]bool{}
	for _, f := range rows {
		if f["draft"] == d["id"] {
			gists[text(f["gist"])] = true
		}
	}
	open, err := s.open(ctx)
	if err != nil {
		return err
	}
	target := false
	for _, item := range fleet.Items(open["drafts"]) {
		if record(item)["id"] == d["id"] {
			target = true
			gists[text(open["gist"])] = true
		}
	}
	for gist := range gists {
		if _, err := s.service.options.Records.DB.ExecContext(ctx, "DELETE FROM intake_error_failures WHERE gist=?", gist); err != nil {
			return err
		}
	}
	if s.checking {
		s.outcomes[text(d["id"])] = Record{"ok": true}
	}
	if target {
		if err := s.resolve(ctx, open, "fixed"); err != nil {
			return err
		}
	}
	return s.snapshot(ctx)
}
func (s *ErrorStalls) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tick(ctx)
}
func (s *ErrorStalls) tick(ctx context.Context) error {
	now := s.service.options.Now()
	if _, err := s.service.options.Records.DB.ExecContext(ctx, "DELETE FROM intake_error_failures WHERE at<?", timestamp(now.Add(-24*time.Hour))); err != nil {
		return err
	}
	open, err := s.open(ctx)
	if err != nil {
		return err
	}
	if open != nil {
		all := true
		for _, item := range fleet.Items(open["drafts"]) {
			gone, err := s.gone(ctx, text(record(item)["id"]))
			if err != nil {
				return err
			}
			all = all && gone
		}
		if all && !s.checking {
			if err := s.resolve(ctx, open, "gone"); err != nil {
				return err
			}
		}
		return s.snapshot(ctx)
	}
	failures, err := s.rows(ctx, "intake_error_failures")
	if err != nil {
		return err
	}
	groups := map[string][]Record{}
	order := []string{}
	for _, f := range failures {
		gist := text(f["gist"])
		if groups[gist] == nil {
			order = append(order, gist)
		}
		groups[gist] = append(groups[gist], f)
	}
	for _, gist := range order {
		list := groups[gist]
		first, err := time.Parse(time.RFC3339Nano, text(list[0]["at"]))
		if err != nil {
			return err
		}
		if now.Sub(first) < 15*time.Minute || len(list) < 2 {
			continue
		}
		alive := false
		for _, f := range list {
			gone, err := s.gone(ctx, text(f["draft"]))
			if err != nil {
				return err
			}
			alive = alive || !gone
		}
		if alive {
			rows, err := s.rows(ctx, "intake_error_stalls")
			if err != nil {
				return err
			}
			series := int64(0)
			for _, r := range rows {
				n, _ := integer(r["series"])
				if n > series {
					series = n
				}
			}
			if _, err := s.record(ctx, list, gist, "同じ相談処理の例外が繰り返され、15分以上解消していません", series+1, 0); err != nil {
				return err
			}
			break
		}
	}
	return s.snapshot(ctx)
}
func (s *ErrorStalls) record(ctx context.Context, list []Record, gist, reason string, series, failures int64) (Record, error) {
	now := s.service.options.Now()
	id := "intake@" + timestamp(now)
	for {
		var n int
		err := s.service.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT COUNT(*) FROM intake_error_stalls WHERE id=?"), id).Scan(&n)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		now = now.Add(time.Millisecond)
		id = "intake@" + timestamp(now)
	}
	drafts := []any{}
	byID := map[string]int{}
	for _, f := range list {
		entry := Record{"id": f["draft"], "project": f["project"], "error": f["error"], "stage": f["stage"], "at": f["at"]}
		if i, ok := byID[text(f["draft"])]; ok {
			drafts[i] = entry
		} else {
			byID[text(f["draft"])] = len(drafts)
			drafts = append(drafts, entry)
		}
	}
	d := Record{"id": id, "series": series, "gist": gist, "reason": reason, "drafts": drafts, "startedAt": list[0]["at"], "detectedAt": timestamp(now), "failures": failures, "autoFixStopped": failures >= 3, "resolvedAt": nil, "result": nil, "revision": s.revision, "checkedRevision": nil, "resolvedRevision": nil, "lastRead": list[len(list)-1]["lastRead"], "unread": list[len(list)-1]["unread"]}
	if err := s.save(ctx, d); err != nil {
		return nil, err
	}
	for _, f := range list {
		if _, err := s.service.options.Records.DB.ExecContext(ctx, "DELETE FROM intake_error_failures WHERE gist=?", f["gist"]); err != nil {
			return nil, err
		}
	}
	return d, nil
}
func (s *ErrorStalls) snapshot(ctx context.Context) error {
	rows, err := s.rows(ctx, "intake_error_stalls")
	if err != nil || len(rows) == 0 {
		return err
	}
	latest := rows[len(rows)-1]
	for _, row := range rows {
		if row["resolvedAt"] == nil {
			latest = row
			break
		}
	}
	dir := filepath.Join(s.runtime, "state")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".intake-stall-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	snapshot := Record{}
	for _, key := range []string{"id", "series", "failures", "autoFixStopped", "startedAt", "detectedAt", "resolvedAt", "result", "gist", "reason", "lastRead", "unread"} {
		snapshot[key] = latest[key]
	}
	snapshot["draftCount"] = len(fleet.Items(latest["drafts"]))
	summaries := []any{}
	for _, item := range fleet.Items(latest["drafts"]) {
		row := record(item)
		summaries = append(summaries, Record{"project": row["project"], "error": row["error"]})
	}
	snapshot["drafts"] = summaries
	raw, err := json.Marshal(snapshot)
	if err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, filepath.Join(dir, "intake-stall.json"))
}
func (s *ErrorStalls) Alerts(ctx context.Context) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.rows(ctx, "intake_error_stalls")
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range rows {
		resolved, _ := time.Parse(time.RFC3339Nano, text(row["resolvedAt"]))
		fixed := row["result"] == "fixed" && s.service.options.Now().Sub(resolved) < 24*time.Hour
		stopped := row["autoFixStopped"] == true && row["resolvedAt"] == nil
		if fixed || stopped {
			entry := clone(row)
			entry["kind"] = "intake_error"
			entry["status"] = "intake_error_stopped"
			entry["title"] = fmt.Sprintf("相談処理の例外: %v", row["gist"])
			if fixed {
				entry["status"] = "intake_error_fixed"
				entry["title"] = "相談処理の例外が解消しました"
			}
			result = append(result, map[string]any(entry))
		}
	}
	return result, nil
}
