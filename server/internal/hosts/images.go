package hosts

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"sort"
	"strings"
)

func (s *Service) imageSnapshots(ctx context.Context, live map[string]map[string]any) (map[string]any, []any, error) {
	read := func(query string) ([]map[string]any, error) {
		rows, err := s.DB.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		out, _, err := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
		return out, err
	}
	releases, err := read("SELECT jsonb_build_object('host',host,'flavor',flavor,'tag',tag,'revision',revision)FROM releases")
	if err != nil {
		return nil, nil, err
	}
	rollouts, err := read("SELECT row_to_json(r)FROM(SELECT DISTINCT ON(flavor)*FROM release_rollouts WHERE host='*'ORDER BY flavor,id DESC)r")
	if err != nil {
		return nil, nil, err
	}
	results, err := read("SELECT jsonb_build_object('host',host,'tag',tag,'flavor',flavor,'status',status,'at',at)FROM host_image_results ORDER BY at DESC,tag DESC")
	if err != nil {
		return nil, nil, err
	}
	history, err := read("SELECT jsonb_build_object('host',host,'tag',tag,'flavor',flavor,'stage',stage,'reason',reason,'detail',detail,'selfCheckFailed',self_check_failed) FROM (SELECT DISTINCT ON (host,tag,flavor) * FROM host_image_history WHERE status='failed' ORDER BY host,tag,flavor,received_at DESC,id DESC) h")
	if err != nil {
		return nil, nil, err
	}
	historyIndex := map[imageHistoryKey]map[string]any{}
	for _, row := range history {
		historyIndex[imageHistoryKey{fleet.Text(row["host"]), fleet.Text(row["tag"]), fleet.Text(row["flavor"])}] = row
	}
	resultIndex := indexImageResults(results)
	stages := map[string]map[string]any{}
	views := []any{}
	for _, r := range rollouts {
		if r["status"] == "superseded" {
			continue
		}
		stage := map[string]any{"flavor": r["flavor"], "stage": r["status"], "revision": r["revision"], "tag": imageTag(fleet.Text(r["revision"]), fleet.Text(r["flavor"])), "canaryHost": r["canary"], "successCycles": r["success_cycles"], "successNeeded": 3, "startedAt": r["started_at"], "decidedAt": r["decided_at"], "reason": r["reason"]}
		stages[fleet.Text(r["flavor"])] = stage
		views = append(views, stage)
	}
	out := map[string]any{}
	for host, status := range live {
		health := fleet.Object(status["hostd_health"])
		current := fleet.Text(fleet.Object(fleet.Object(health["cycle"])["versions"])["current"])
		flavor := ""
		if strings.HasSuffix(current, "-python") {
			flavor = "-python"
		}
		release, desired := desiredImage(host, flavor, releases, rollouts)
		failed, shown := resultIndex.forHost(host, flavor)
		rollout := imageRollout(host, release, stages[flavor])
		out[host] = map[string]any{"image": map[string]any{"flavor": flavor, "desired": desired, "failedTags": failed, "results": shown, "lastFailure": imageFailureWithHistory(shown, status, host, historyIndex)}, "rollout": rollout}
	}
	return out, views, nil
}

// imageTag は revision と種類から、イメージのタグを作る。
func imageTag(revision, flavor string) string {
	tag := revision
	if len(tag) > 12 {
		tag = tag[:12]
	}
	if flavor == "-python" {
		tag += "-client-python"
	}
	return tag
}

// desiredImage はPCの種類ごとに適用される版の登録と、先行PCの段取りを踏まえてそのPCが取るべきイメージを決める。
func desiredImage(host, flavor string, releases, rollouts []map[string]any) (map[string]any, any) {
	var release map[string]any
	for _, r := range releases {
		if r["flavor"] == flavor && (r["host"] == host || r["host"] == "*" && (release == nil || release["host"] != host)) {
			release = r
		}
	}
	var desired any
	var active map[string]any
	for _, r := range rollouts {
		if r["flavor"] == flavor {
			active = r
		}
	}
	if release != nil {
		chosen := release
		previous := false
		if release["host"] == "*" && active != nil && active["revision"] == release["revision"] && (active["status"] == "failed" || active["status"] == "canary" && active["canary"] != host) {
			chosen = fleet.Object(active["previous"])
			previous = true
		}
		if chosen != nil {
			role := "desired"
			var reason any
			if previous {
				role = "previous"
				reason = fleet.Text(active["status"])
				if fleet.Text(active["reason"]) != "" {
					reason = reason.(string) + ":" + fleet.Text(active["reason"])
				}
			} else if release["host"] == "*" && active["revision"] == release["revision"] && active["status"] == "canary" && active["canary"] == host {
				role = "canary"
			}
			source := "host"
			if release["host"] == "*" {
				source = "global"
			}
			tag := chosen["tag"]
			if tag == nil {
				tag = imageTag(fleet.Text(chosen["revision"]), flavor)
			}
			desired = map[string]any{"tag": tag, "revision": chosen["revision"], "source": source, "role": role, "reason": reason}
		}
	}
	return release, desired
}

// imageRollout はPCから見た先行PCの段取りの状態（広げる・先行・保留と、その対象外の理由）を作る。
func imageRollout(host string, release, stage map[string]any) map[string]any {
	rollout := map[string]any{"role": nil, "stage": "none", "revision": nil, "tag": nil, "canaryHost": nil, "successCycles": nil, "successNeeded": nil, "startedAt": nil, "decidedAt": nil, "reason": nil}
	if stage != nil {
		rollout = fleet.Object(fleet.Clone(stage))
		delete(rollout, "flavor")
		// Progress remains visible even when this rollout does not select
		// the host's effective desired registration.
		var role any
		reason := ""
		switch {
		case release == nil:
			reason = "release_missing"
		case release["host"] != "*":
			reason = "host_override"
		case release["revision"] != stage["revision"]:
			reason = "global_revision_changed"
		case stage["stage"] == "passed":
			role = "spread"
		case stage["canaryHost"] == host:
			role = "canary"
		default:
			role = "held"
		}
		rollout["role"] = role
		if reason != "" {
			if detail := fleet.Text(stage["reason"]); detail != "" {
				reason += ":" + detail
			}
			rollout["reason"] = reason
		}
	}
	return rollout
}

type imageResultKey struct{ tag, flavor any }

type imageResultIndex struct {
	recent  map[string][]map[string]any
	failed  map[string]map[any][]any
	success map[imageResultKey][]string
}

// Results arrive in database read order; retaining that order preserves ties.
func indexImageResults(results []map[string]any) imageResultIndex {
	idx := imageResultIndex{recent: map[string][]map[string]any{}, failed: map[string]map[any][]any{}, success: map[imageResultKey][]string{}}
	seen := map[imageResultKey]map[string]bool{}
	for _, r := range results {
		host := fleet.Text(r["host"])
		if len(idx.recent[host]) < 5 {
			idx.recent[host] = append(idx.recent[host], r)
		}
		if r["status"] == "failed" {
			if idx.failed[host] == nil {
				idx.failed[host] = map[any][]any{}
			}
			idx.failed[host][r["flavor"]] = append(idx.failed[host][r["flavor"]], r["tag"])
		}
		if r["status"] == "success" {
			key := imageResultKey{r["tag"], r["flavor"]}
			if seen[key] == nil {
				seen[key] = map[string]bool{}
			}
			if !seen[key][host] {
				seen[key][host] = true
				idx.success[key] = append(idx.success[key], host)
			}
		}
	}
	for _, names := range idx.success {
		sort.Strings(names)
	}
	return idx
}

func (idx imageResultIndex) forHost(host, flavor string) ([]any, []any) {
	failed := append([]any{}, idx.failed[host][flavor]...)
	shown := []any{}
	for _, r := range idx.recent[host] {
		succeeded := []string{}
		for _, name := range idx.success[imageResultKey{r["tag"], r["flavor"]}] {
			if name != host {
				succeeded = append(succeeded, name)
			}
		}
		shown = append(shown, map[string]any{"tag": r["tag"], "flavor": r["flavor"], "status": r["status"], "at": r["at"], "succeededOn": succeeded})
	}
	return failed, shown
}

func imageFailure(results []any, status map[string]any) any {
	if len(results) == 0 {
		return nil
	}
	latest := fleet.Object(results[0])
	if latest["status"] != "failed" {
		return nil
	}
	health := fleet.Object(publicHealth(map[string]any{"cycle": fleet.Object(status["hostd_health"])["cycle"]}))
	cycle := fleet.Object(health["cycle"])
	out := map[string]any{"tag": latest["tag"], "at": latest["at"], "stage": nil, "reason": nil, "detail": nil, "cycleAt": nil, "selfCheckFailed": []any{}}
	if cycle["result"] == "failed" {
		out["stage"], out["detail"], out["cycleAt"] = cycle["stage"], cycle["detail"], cycle["at"]
		if reason := fleet.Text(cycle["reason"]); reason != "" {
			if cycle["httpStatus"] != nil {
				reason += fmt.Sprintf("（HTTP %v）", cycle["httpStatus"])
			}
			out["reason"] = reason
		}
	}
	for _, raw := range fleet.List(fleet.Object(status["checks"])["items"]) {
		item := fleet.Object(raw)
		if item["ok"] == false && len(fleet.List(out["selfCheckFailed"])) < 10 {
			label := fleet.Text(item["label"])
			if label == "" {
				label = fleet.Text(item["name"])
			}
			out["selfCheckFailed"] = append(fleet.List(out["selfCheckFailed"]), clip(usage.Redact(label), 300))
		}
	}
	return out
}

// imageHistoryKey identifies the result whose failure details were reported.
type imageHistoryKey struct{ host, tag, flavor string }

func imageFailureWithHistory(results []any, status map[string]any, host string, history map[imageHistoryKey]map[string]any) any {
	failure := fleet.Object(imageFailure(results, status))
	if failure == nil {
		return nil
	}
	latest := fleet.Object(results[0])
	row := history[imageHistoryKey{host, fleet.Text(latest["tag"]), fleet.Text(latest["flavor"])}]
	if row == nil || fleet.Text(row["reason"]) == "" {
		return failure
	}
	for _, key := range []string{"stage", "reason", "detail"} {
		failure[key] = nil
		if value, ok := row[key].(string); ok {
			failure[key] = clip(usage.Redact(value), 300)
		}
	}
	checks := []any{}
	for _, item := range fleet.List(row["selfCheckFailed"]) {
		if len(checks) == 10 {
			break
		}
		if value, ok := item.(string); ok {
			checks = append(checks, clip(usage.Redact(value), 300))
		}
	}
	failure["selfCheckFailed"] = checks
	return failure
}
