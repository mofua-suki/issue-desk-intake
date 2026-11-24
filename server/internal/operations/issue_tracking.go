package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

// IssueTracking observes jobs; it never grants approval or invokes retry.
type IssueTracking struct {
	Service     *Service
	List        func(context.Context, string, []string, int, int) ([]*jobs.Job, int, error)
	ReadJob     func(context.Context, string, int64) (*jobs.Job, error)
	Detail      func(context.Context, string, int64) (map[string]any, error)
	ReadTarget  func(context.Context, string, int64) (map[string]any, error)
	PendingStop func(context.Context, *jobs.Job) (bool, error)
	Pull        func(context.Context, string, int64) (map[string]any, error)
	Notify      func(context.Context, map[string]any) (map[string]any, error)
	// Reserve is used to substitute persistent storage in an isolated test.
	Reserve func(context.Context, *jobs.Job, []byte) (bool, error)
}

type trackingEvent struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Action     string `json:"action,omitempty"`
	Generation int64  `json:"generation"`
	Revision   string `json:"revision,omitempty"`
}

var trackingSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Reconcile reads jobs outside the notification reservation transaction. A failed
// read does not remove or complete any earlier observation.
func (t *IssueTracking) Reconcile(ctx context.Context) error {
	if t.Service == nil || t.List == nil || t.ReadJob == nil || t.Detail == nil || t.Pull == nil {
		return errors.New("追跡の取得口がありません")
	}
	var repos map[string]bool
	if t.Service.Repositories != nil {
		rs, err := t.Service.Repositories()
		if err != nil {
			return err
		}
		repos = map[string]bool{}
		for _, r := range rs {
			repos[r] = true
		}
	}
	var all []*jobs.Job
	for offset := 0; ; {
		page, total, err := t.List(ctx, "", []string{"queued", "running", "held", "failed", "stopped", "merged", "closed"}, 1000, offset)
		if err != nil {
			return err
		}
		all = append(all, page...)
		offset += len(page)
		if offset >= total {
			break
		}
		if len(page) == 0 {
			return errors.New("追跡一覧が途中で終了しました")
		}
	}
	var problems []error
	for _, item := range all {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(problems, err)...)
		}
		if item == nil || item.Kind == "investigation" || repos != nil && !repos[item.Repo] {
			continue
		}
		j, err := t.ReadJob(ctx, item.Repo, item.Issue)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if j == nil || j.ID != item.ID {
			problems = append(problems, errors.New("追跡対象を照合できません"))
			continue
		}
		var observedStop *bool
		checkStop := j.Status == "merged" || j.Status == "closed"
		if !checkStop && j.LeaseHost != nil && t.PendingStop != nil {
			checkStop, err = t.PendingStop(ctx, j)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if !checkStop {
				stopped := j.Status == "stopped" || j.View["stopRequested"] == true
				observedStop = &stopped
			}
		}
		if checkStop && j.LeaseHost != nil {
			if t.ReadTarget == nil {
				problems = append(problems, errors.New("停止指示を照合できません"))
				continue
			}
			target, err := t.ReadTarget(ctx, j.Repo, j.Issue)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			stopped, ok := target["stopped"].(bool)
			if !ok {
				problems = append(problems, errors.New("停止状態が未確認です"))
				continue
			}
			observedStop = &stopped
		}
		if j.Status == "merged" && j.View["stopRequested"] != true && (observedStop == nil || !*observedStop) && t.Reserve == nil {
			value, err := t.Service.OperationalSetting(ctx, trackingKey(j))
			if err != nil {
				problems = append(problems, err)
				continue
			}
			previous := fleet.Object(value)
			if previous["state"] == "merged" && previous["revision"] == trackingRevision(j) && previous["pending"] != true {
				continue
			}
		}
		var event trackingEvent
		if observedStop != nil && *observedStop {
			event = trackingEvent{State: "stopped", Reason: "停止指示を確認しました", Actor: "受付・担当AI", Action: "停止理由と再開条件を確認する", Generation: j.Generation}
		} else {
			event, err = t.event(ctx, j)
		}
		if err != nil {
			problems = append(problems, err)
			continue
		}
		latest, err := t.ReadJob(ctx, j.Repo, j.Issue)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !sameTrackedJob(j, latest) {
			problems = append(problems, errors.New("追跡対象が確認中に変化しました"))
			continue
		}
		added, err := t.reserve(ctx, j, event, observedStop)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if added && event.State != "running" && event.State != "queued" && t.Notify != nil {
			result, notifyErr := t.Notify(ctx, map[string]any{"title": "依頼の追跡: " + event.State, "body": usage.Redact(fmt.Sprintf("%s#%d %s 理由: %s 次: %s %s", j.Repo, j.Issue, j.Title, event.Reason, event.Actor, event.Action)), "url": "/#/list/attention"})
			err = notifyErr
			if err == nil && result != nil {
				if _, ok := result["sent"]; ok && (fleet.WholeNumber(result["sent"]) == 0 || fleet.WholeNumber(result["failed"]) > 0) {
					err = errors.New("追跡通知を配信できませんでした")
				}
			}
			if err != nil {
				problems = append(problems, err)
			} else if t.Reserve == nil {
				if err = t.delivered(ctx, j, event); err != nil {
					problems = append(problems, err)
				}
			}
		}
	}
	return errors.Join(problems...)
}

func sameTrackedJob(a, b *jobs.Job) bool {
	if a == nil || b == nil || a.ID != b.ID || a.Repo != b.Repo || a.Issue != b.Issue || a.Status != b.Status || a.Generation != b.Generation || a.UpdatedAt != b.UpdatedAt {
		return false
	}
	eq := func(x, y *string) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return eq(a.Branch, b.Branch) && eq(a.HeadSHA, b.HeadSHA) && (a.PR == nil && b.PR == nil || a.PR != nil && b.PR != nil && *a.PR == *b.PR)
}

func (t *IssueTracking) event(ctx context.Context, j *jobs.Job) (trackingEvent, error) {
	e := trackingEvent{State: j.Status, Generation: j.Generation}
	if j.View["stopRequested"] == true {
		e.State = "stopped"
	}
	switch e.State {
	case "merged":
		e.Revision = trackingRevision(j)
		e.State = "unconfirmed"
		e.Reason = "対象PRのマージを確認できません"
		e.Actor = "受付"
		e.Action = "対象PRと結果を確認する"
		if j.PR == nil || *j.PR <= 0 || j.HeadSHA == nil || j.Branch == nil || !trackingSHA.MatchString(*j.HeadSHA) || *j.Branch == "" {
			break
		}
		p, err := t.Pull(ctx, j.Repo, *j.PR)
		if err != nil {
			return e, err
		}
		head, base := fleet.Object(p["head"]), fleet.Object(p["base"])
		hr, br := fleet.Object(head["repo"]), fleet.Object(base["repo"])
		branch := fleet.Text(br["default_branch"])
		if fleet.WholeNumber(p["number"]) == *j.PR && p["merged"] == true && trackingSHA.MatchString(fleet.Text(p["merge_commit_sha"])) && head["sha"] == *j.HeadSHA && head["ref"] == *j.Branch && hr["full_name"] == j.Repo && br["full_name"] == j.Repo && branch != "" && base["ref"] == branch {
			e.State = "merged"
			e.Reason = "対象PRのマージを確認"
			e.Actor = "なし"
			e.Action = "完了"
		}
	case "closed":
		d, err := t.Detail(ctx, j.Repo, j.Issue)
		if err != nil {
			return e, err
		}
		reason := fleet.Object(d["closeReason"])
		if d["state"] == "closed" && reason["reason"] == "voluntary" && (reason["source"] == "manual" || reason["source"] == "token") {
			e.State = "withdrawn"
			e.Reason = "明示的な取り下げ"
			e.Actor = "なし"
			e.Action = "終了"
		} else {
			e.State = "unconfirmed"
			e.Reason = "終了理由またはマージ根拠が未確認"
			e.Actor = "受付"
			e.Action = "終了理由と対象PRを確認する"
		}
	case "held", "failed", "stopped":
		d, err := t.Detail(ctx, j.Repo, j.Issue)
		if err != nil {
			return e, err
		}
		e.Reason = fleet.Text(d["error"])
		if e.Reason == "" {
			e.Reason = fleet.Text(j.Result["reason"])
		}
		if e.Reason == "" && j.HoldReason != nil {
			e.Reason = *j.HoldReason
		}
		if e.Reason == "" {
			e.Reason = "原因を確認できません"
		}
		e.Actor = "受付・担当AI"
		e.Action = "原因と再開条件を確認する"
		if e.State != "stopped" && d["requiresUser"] == true {
			e.State = "waiting"
			e.Actor = "利用者"
			e.Action = fleet.Text(d["nextAction"])
			if e.Action == "" {
				e.Action = "質問または承認を確認する"
			}
		}
	case "queued", "running":
		if j.ProgressAt != nil {
			if at, err := time.Parse(time.RFC3339Nano, *j.ProgressAt); err == nil && t.Service.now().Sub(at) >= time.Hour {
				e.State = "stalled"
				e.Reason = "60分以上進捗の更新がありません"
				e.Actor = "受付・担当AI"
				e.Action = "状況と再開条件を確認する"
			}
		}
	default:
		e.State = "unconfirmed"
		e.Reason = "状態が未確認"
		e.Actor = "受付"
		e.Action = "依頼の状態を確認する"
	}
	return e, nil
}

func (t *IssueTracking) reserve(ctx context.Context, j *jobs.Job, event trackingEvent, observedStop *bool) (bool, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	if t.Reserve != nil {
		return t.Reserve(ctx, j, raw)
	}
	if t.Service.PostgreSQL == nil {
		return false, errors.New("追跡の保存先がありません")
	}
	tx, err := t.Service.PostgreSQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	view, err := json.Marshal(j.View)
	if err != nil {
		return false, err
	}
	result, err := json.Marshal(j.Result)
	if err != nil {
		return false, err
	}
	hold := j.HoldReason
	if hold != nil && *hold == "prerequisite_missing" {
		stored := "question"
		hold = &stored
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id=$1 AND repo=$2 AND issue=$3 AND generation=$4 AND status=$5 AND pr IS NOT DISTINCT FROM $6 AND head_sha IS NOT DISTINCT FROM $7 AND branch IS NOT DISTINCT FROM $8 AND date_trunc('milliseconds',updated_at)=$9::timestamptz AND hold_reason IS NOT DISTINCT FROM $10 AND date_trunc('milliseconds',progress_at) IS NOT DISTINCT FROM $11::timestamptz AND (view - 'diffFingerprint')=($12::jsonb - 'diffFingerprint') AND result IS NOT DISTINCT FROM NULLIF($13::jsonb,'null'::jsonb) AND lease_host IS NOT DISTINCT FROM $14 FOR UPDATE`, j.ID, j.Repo, j.Issue, j.Generation, j.Status, j.PR, j.HeadSHA, j.Branch, j.UpdatedAt, hold, j.ProgressAt, string(view), string(result), j.LeaseHost).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errors.New("追跡対象が通知予約前に変化しました")
	}
	if err != nil {
		return false, err
	}
	if observedStop != nil {
		var stopped bool
		err = tx.QueryRowContext(ctx, `SELECT status='stopped' OR COALESCE(view->'stopRequested'='true'::jsonb,false) OR EXISTS(SELECT 1 FROM job_commands c WHERE c.target_host=jobs.lease_host AND c.repo=jobs.repo AND c.issue=jobs.issue AND c.kind='stop' AND c.status IN ('queued','acked')) FROM jobs WHERE id=$1`, j.ID).Scan(&stopped)
		if err != nil {
			return false, err
		}
		if stopped != *observedStop {
			return false, errors.New("停止指示が通知予約前に変化しました")
		}
	}
	key := trackingKey(j)
	needsNotice := event.State != "running" && event.State != "queued" && t.Notify != nil
	var same, pending bool
	err = tx.QueryRowContext(ctx, `SELECT value - 'revision' - 'pending' = $2::jsonb - 'revision', COALESCE((value->>'pending')::boolean, false) FROM settings WHERE key=$1 FOR UPDATE`, key, string(raw)).Scan(&same, &pending)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if same && pending {
		needsNotice = true
	}
	if same && !pending {
		needsNotice = false
	}
	var stored map[string]any
	if err = json.Unmarshal(raw, &stored); err != nil {
		return false, err
	}
	stored["pending"] = needsNotice
	raw, err = json.Marshal(stored)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,expires_at,updated_at,updated_by) VALUES($1,$2::jsonb,NULL,$3,'issue-tracking') ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires_at=NULL,updated_at=excluded.updated_at`, key, string(raw), t.Service.now())
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return needsNotice, nil
}

// delivered only marks the event that was actually sent; a newer observation wins.
func (t *IssueTracking) delivered(ctx context.Context, j *jobs.Job, event trackingEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = t.Service.PostgreSQL.ExecContext(ctx, `UPDATE settings SET value=jsonb_set(value, '{pending}', 'false'::jsonb), updated_at=$3 WHERE key=$1 AND value - 'pending' = $2::jsonb`, trackingKey(j), string(raw), t.Service.now())
	return err
}

func trackingKey(j *jobs.Job) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", j.Repo, j.Issue)))
	return "issue-tracking:" + hex.EncodeToString(hash[:])
}

func trackingRevision(j *jobs.Job) string {
	raw, _ := json.Marshal([]any{j.ID, j.Generation, j.Status, j.UpdatedAt, j.PR, j.HeadSHA, j.Branch})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
