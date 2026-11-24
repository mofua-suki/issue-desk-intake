package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
)

// PermissionApproval only accepts the fully verified server-side target. Audit
// receipts are preserved separately from permissionHold and deletionApproved.
func (s *Store) PermissionApproval(ctx context.Context, id int64, identity, hold map[string]any, finish bool) (*Job, error) {
	var result *Job
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		job, e := readJob(ctx, tx, id, true)
		if e != nil {
			return e
		}
		operation := fleet.Text(identity["id"])
		if !regexpUUID.MatchString(operation) {
			return controlProblem(400, "承認IDが不正です。")
		}
		if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "permission-approval:"+operation); e != nil {
			return e
		}
		var collision int64
		e = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE view->'permissionApproval'->>'id'=$1 AND id<>$2 LIMIT 1`, operation, id).Scan(&collision)
		if e == nil {
			return controlProblem(409, "送信IDは別の承認で使用済みです。")
		}
		if e != sql.ErrNoRows {
			return e
		}
		previous, replayed, e := previousPermissionApproval(job, operation, identity)
		if e != nil {
			return e
		}
		if replayed {
			result = job
			return nil
		}
		if e = permissionHoldMatches(job, hold); e != nil {
			return e
		}
		if len(previous) > 0 && (previous["head"] != hold["head"] || fleet.Number(previous["pr"]) != fleet.Number(hold["pr"]) || !reflect.DeepEqual(previous["files"], hold["files"])) {
			return controlProblem(409, "保存済みの承認対象と完全な集合が一致しません。")
		}
		if len(previous) == 0 {
			previous = map[string]any{}
			for k, v := range identity {
				previous[k] = v
			}
			for _, k := range []string{"head", "pr", "files"} {
				previous[k] = hold[k]
			}
			previous["at"] = iso(s.options.Now())
			previous["resumePublication"] = false
		}
		if finish {
			if e = resumePermissionPublication(job, hold, previous); e != nil {
				return e
			}
		}
		job.View["permissionApproval"] = previous
		raw, _ := json.Marshal(job.View)
		if _, e = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,status=$3,hold_reason=$4,updated_at=$5 WHERE id=$1`, id, string(raw), job.Status, job.HoldReason, s.options.Now()); e != nil {
			return e
		}
		if e = addEvent(ctx, tx, id, fleet.Text(identity["approvedBy"]), "permission_approval", map[string]any{"id": operation, "head": hold["head"], "files": hold["files"], "resumePublication": finish}, s.options.Now()); e != nil {
			return e
		}
		result, e = readJob(ctx, tx, id, false)
		return e
	})
	return result, e
}

// previousPermissionApproval は記録済みの権限変更の承認が同じ操作の続きか、監査投稿を終えた前の承認かを確かめる。公開を再開済みの送り直しなら replayed を返す。
func previousPermissionApproval(job *Job, operation string, identity map[string]any) (map[string]any, bool, error) {
	previous := fleet.Object(job.View["permissionApproval"])
	if len(previous) > 0 {
		if previous["id"] != operation || !controlSameJSON(previous["identity"], identity["identity"]) {
			if previous["id"] == operation || previous["issuePosted"] != true || previous["prPosted"] != true || job.Status != "held" {
				return nil, false, controlProblem(409, "別の承認操作が記録されています。")
			}
			previous = map[string]any{}
		} else if previous["resumePublication"] == true && job.Status != "held" {
			return previous, true, nil
		}
	}
	return previous, false, nil
}

// permissionHoldMatches は承認対象がレビュー保留のジョブの今のPR・SHA・世代・lease と一致し、権限ファイルの集合を持つかを確かめる。
func permissionHoldMatches(job *Job, hold map[string]any) error {
	if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held" || job.PR == nil || job.HeadSHA == nil || *job.PR != int64(fleet.Number(hold["pr"])) || *job.HeadSHA != hold["head"] || job.Generation != int64(fleet.Number(hold["generation"])) || job.LeaseEpoch != int64(fleet.Number(hold["epoch"])) || !strings.EqualFold(job.Repo, fleet.Text(hold["project"])) {
		return controlProblem(409, "承認対象のジョブが変わりました。")
	}
	if len(fleet.List(hold["files"])) == 0 {
		return controlProblem(409, "権限ファイルの完全な集合が必要です。")
	}
	return nil
}

// resumePermissionPublication は監査投稿を終えた承認で、検証済みの成果物の公開を再開できるようにして待ちへ戻す。
func resumePermissionPublication(job *Job, hold, previous map[string]any) error {
	if previous["issuePosted"] != true || previous["prPosted"] != true {
		return controlProblem(409, "承認の監査投稿が完了していません。")
	}
	if artifact, ok := artifactOf(job); ok && artifact.Generation == job.Generation && artifact.Branch != "" && artifact.Head == hold["head"] {
		continuation := fleet.Object(job.View["continuation"])
		continuation["publication"] = map[string]any{"generation": artifact.Generation, "branch": artifact.Branch, "headSha": artifact.Head, "verified": true}
		job.View["continuation"] = continuation
	}
	previous["resumePublication"] = true
	job.Status = "queued"
	job.HoldReason = nil
	return nil
}

func (s *Store) PermissionReceipt(ctx context.Context, id int64, operation string, patch map[string]any) (*Job, error) {
	var result *Job
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		job, e := readJob(ctx, tx, id, true)
		if e != nil {
			return e
		}
		record := fleet.Object(job.View["permissionApproval"])
		if record["id"] != operation {
			return controlProblem(409, "承認操作が変わりました。")
		}
		for key, value := range patch {
			if !oneOf(key, "issuePosted", "prPosted", "issueAttempted", "prAttempted", "issueComment", "prComment") {
				return controlProblem(400, "監査項目が不正です。")
			}
			record[key] = value
		}
		job.View["permissionApproval"] = record
		raw, _ := json.Marshal(job.View)
		if _, e = tx.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,updated_at=$3 WHERE id=$1`, id, string(raw), s.options.Now()); e != nil {
			return e
		}
		result, e = readJob(ctx, tx, id, false)
		return e
	})
	return result, e
}

func controlSameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
