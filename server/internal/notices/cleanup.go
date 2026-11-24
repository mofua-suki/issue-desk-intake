package notices

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"sync"
	"time"
)

type Cleanup struct {
	Service    *Service
	PostgreSQL *sql.DB
	ReadIssue  func(context.Context, string, int64) (map[string]any, error)
	mu         sync.Mutex
	last       time.Time
}

func (c *Cleanup) Tick(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Service.now()
	if !c.last.IsZero() && now.Sub(c.last) >= 0 && now.Sub(c.last) < 10*time.Minute {
		return nil
	}
	c.last = now
	return c.Reconcile(ctx)
}
func (c *Cleanup) Reconcile(ctx context.Context) error {
	remove, e := c.selectNoticeRemovals(ctx)
	if e != nil {
		return e
	}
	return c.archiveNoticeRemovals(ctx, remove)
}

// selectNoticeRemovals は運用の通知のうち、Issueが解決した・再試行された・Issueなしで7日たったものを選び、行の指紋ごとの理由を返す。
func (c *Cleanup) selectNoticeRemovals(ctx context.Context) (map[string]string, error) {
	s := c.Service
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	snapshot, e := (settings{conn, ctx}).read("ops-watch-notices")
	conn.Close()
	if e != nil {
		return nil, e
	}
	remove := map[string]string{}
	type reading struct {
		resolved bool
		retry    time.Time
		valid    bool
	}
	cache := map[string]reading{}
	for _, raw := range snapshot {
		row := fleet.Object(raw)
		at, ok := timestamp(row["updatedAt"])
		if !ok || contract.ISO(at) != row["updatedAt"] {
			continue
		}
		targets, ok := row["issues"].([]any)
		if !ok || len(targets) > 1 {
			continue
		}
		if len(targets) == 0 {
			if s.now().Sub(at) >= 7*24*time.Hour {
				remove[encode(row)] = "expired_without_issue"
			}
			continue
		}
		target := fleet.Object(targets[0])
		repo := fleet.Text(target["repo"])
		number := fleet.Number(target["number"])
		if !fleet.ValidRepo(repo) || number < 1 || number != float64(int64(number)) {
			continue
		}
		key := fmt.Sprintf("%s#%.0f", strings.ToLower(repo), number)
		read, exists := cache[key]
		if !exists {
			read = c.readTarget(ctx, repo, int64(number))
			cache[key] = read
		}
		if !read.valid {
			continue
		}
		if read.resolved {
			remove[encode(row)] = "issue_resolved"
		} else if read.retry.After(at) {
			remove[encode(row)] = "retried"
		}
	}
	return remove, nil
}

// archiveNoticeRemovals は取引の中で、選んだ通知を今の一覧から外して保管庫へ移し、期限切れの保管を捨てる。
func (c *Cleanup) archiveNoticeRemovals(ctx context.Context, remove map[string]string) error {
	s := c.Service
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	if e = sqlDialect.BeginLockedWrite(ctx, conn, "ops-watch-notices"); e != nil {
		return e
	}
	defer conn.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	st := settings{conn, ctx}
	current, e := st.read("ops-watch-notices")
	if e != nil {
		return e
	}
	prior, e := readArchive(st)
	if e != nil {
		return e
	}
	archive := []any{}
	for _, raw := range prior {
		if !expiredArchive(fleet.Object(raw), s.now()) {
			archive = append(archive, raw)
		}
	}
	remaining := []any{}
	for _, raw := range current {
		fingerprint := encode(raw)
		reason, selected := remove[fingerprint]
		if !selected {
			remaining = append(remaining, raw)
			continue
		}
		duplicate := false
		for _, a := range archive {
			row := fleet.Object(a)
			if row["restoredAt"] == nil && encode(row["notice"]) == fingerprint {
				duplicate = true
				break
			}
		}
		if !duplicate {
			id, e := archiveID()
			if e != nil {
				return e
			}
			archive = append(archive, map[string]any{"id": id, "notice": raw, "removedAt": contract.ISO(s.now()), "reason": reason})
		}
	}
	if len(remaining) != len(current) || len(archive) != len(prior) {
		if e = st.save("ops-watch-notices-archive", archive); e != nil {
			return e
		}
	}
	if len(remaining) != len(current) {
		if e = st.save("ops-watch-notices", remaining); e != nil {
			return e
		}
	}
	_, e = conn.ExecContext(ctx, bindSQL("COMMIT"))
	return e
}

func archiveID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func (c *Cleanup) readTarget(ctx context.Context, repo string, number int64) (result struct {
	resolved bool
	retry    time.Time
	valid    bool
}) {
	if c.ReadIssue == nil {
		return
	}
	issue, e := c.ReadIssue(ctx, repo, number)
	if e != nil || fleet.Number(issue["number"]) != float64(number) || issue["pull_request"] != nil || (issue["state"] != "open" && issue["state"] != "closed") {
		return
	}
	if issue["state"] == "closed" {
		result.resolved, result.valid = true, true
		return
	}
	if c.PostgreSQL == nil {
		return
	}
	var id int64
	var status string
	e = c.PostgreSQL.QueryRowContext(ctx, "SELECT id,status FROM jobs WHERE lower(repo)=lower($1)AND issue=$2", repo, number).Scan(&id, &status)
	if e == sql.ErrNoRows {
		result.valid = true
		return
	}
	if e != nil {
		return
	}
	switch status {
	case "merged", "closed":
		result.resolved, result.valid = true, true
		return
	case "queued", "running", "held", "failed", "stopped":
	default:
		return
	}
	rows, e := c.PostgreSQL.QueryContext(ctx, "SELECT at,data FROM job_events WHERE job_id=$1 AND kind='control' ORDER BY seq DESC LIMIT 100", id)
	if e != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var at time.Time
		var raw []byte
		if e = rows.Scan(&at, &raw); e != nil {
			return
		}
		var data map[string]any
		if json.Unmarshal(raw, &data) != nil {
			return
		}
		if data["action"] == "retry" && at.After(result.retry) {
			result.retry = at
		}
	}
	result.valid = rows.Err() == nil
	return
}
