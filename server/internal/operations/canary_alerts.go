package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// canaryOutdated reports whether every PC of the flavor has moved past a failed
// rollout through a per-PC release registered after it. Such a release does not
// touch the host='*' row, so the failed rollout otherwise stays the latest one.
// PCs still on the failed or previous tag keep the alert.
func (s *Service) canaryOutdated(ctx context.Context, r map[string]any, flavor, failedTag, oldTag string, hosts []map[string]any) (bool, error) {
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT host FROM releases WHERE flavor=$1 AND host<>'*' AND created_at>$2::timestamptz AND revision<>$3 AND revision<>$4", flavor, r["started_at"], fleet.Text(r["revision"]), fleet.Text(fleet.Object(r["previous"])["revision"]))
	if e != nil {
		return false, e
	}
	defer rows.Close()
	advanced := map[string]bool{}
	for rows.Next() {
		var host string
		if e = rows.Scan(&host); e != nil {
			return false, e
		}
		advanced[host] = true
	}
	if e = rows.Err(); e != nil {
		return false, e
	}
	seen := 0
	for _, h := range hosts {
		current := fleet.Text(fleet.Object(fleet.Object(fleet.Object(h["hostd_health"])["cycle"])["versions"])["current"])
		if current == "" || strings.HasSuffix(current, "-python") != (flavor == "-python") {
			continue
		}
		seen++
		if !advanced[fleet.Text(h["host"])] || current == failedTag || current == oldTag {
			return false, nil
		}
	}
	return seen > 0, nil
}

func (s *Service) canaryAlerts(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil {
		return out, nil
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT row_to_json(r) FROM(SELECT DISTINCT ON(flavor)* FROM release_rollouts WHERE host='*'ORDER BY flavor,id DESC)r WHERE status='failed'")
	if e != nil {
		return nil, e
	}
	list, _, e := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	if e != nil {
		return nil, e
	}
	hosts, e := s.hostRows(ctx)
	if e != nil {
		return nil, e
	}
	for _, r := range list {
		flavor := fleet.Text(r["flavor"])
		label := flavor
		if label == "" {
			label = "default"
		}
		revision := fleet.Text(r["revision"])
		if len(revision) < 12 {
			continue
		}
		tag := revision[:12]
		if flavor == "-python" {
			tag += "-client-python"
		}
		previous := fleet.Object(r["previous"])
		old := fleet.Text(previous["tag"])
		if old == "" {
			old = fleet.Text(previous["revision"])
		}
		outdated, e := s.canaryOutdated(ctx, r, flavor, tag, old, hosts)
		if e != nil {
			return nil, e
		}
		if outdated {
			continue
		}
		if old == "" {
			old = "不明"
		}
		result := canaryRollbackResult(r, hosts)
		at := r["decided_at"]
		if at == nil {
			at = r["started_at"]
		}
		reason := fleet.Text(r["reason"])
		if reason == "" {
			reason = "不明"
		}
		out = append(out, map[string]any{"id": "release-canary-" + label + "-" + revision[:12], "kind": "hostd_rollback", "status": "stall_alert", "project": fleet.SystemProject, "host": r["canary"], "detectedAt": at, "title": "カナリア配布に失敗しました（" + label + "）", "detail": fmt.Sprintf("カナリア: %v\n新タグ: %s\n旧タグ: %s\n失敗理由: %s\nrollback指示の結果: %s", r["canary"], tag, old, reason, result)})
	}
	return out, nil
}

// canaryRollbackResult は失敗したカナリアへの戻す指示の結果を、そのPCの hostd の報告から探して文にする。
func canaryRollbackResult(r map[string]any, hosts []map[string]any) string {
	result := "指示未発行"
	if r["rollback_command_id"] != nil {
		result = "結果未報告"
		for _, h := range hosts {
			if h["host"] != r["canary"] {
				continue
			}
			commands := fleet.Object(fleet.Object(h["hostd_health"])["commands"])
			for _, raw := range fleet.List(commands["results"]) {
				entry := fleet.Object(raw)
				if entry["id"] == r["rollback_command_id"] {
					result = fleet.Text(entry["result"])
					if result == "" {
						result = "結果あり"
						if entry["ok"] == true {
							result = "success"
						} else if entry["ok"] == false {
							result = "failed"
						}
					}
					if reason := fleet.Text(entry["reason"]); reason != "" {
						result += ": " + reason
					}
				}
			}
		}
	}
	return result
}
