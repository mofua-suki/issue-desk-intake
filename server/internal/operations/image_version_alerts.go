package operations

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostdrift"
	"sort"
	"strings"
	"time"
)

func imageVersionAlerts(view map[string]any, drift *hostdrift.Snapshot) []any {
	if drift == nil || len(drift.Drifts) == 0 {
		return []any{}
	}
	versions := map[string][]string{}
	failed := []string{}
	for _, raw := range fleet.List(view["hosts"]) {
		r := fleet.Object(raw)
		if r["alive"] != true {
			continue
		}
		host := fleet.Text(r["name"])
		if revision := fleet.Text(r["imageRevision"]); revision != "" {
			versions[revision] = append(versions[revision], host)
		}
		image := fleet.Object(r["image"])
		if image != nil {
			tags := strings.Join(fleet.Strings(image["failedTags"]), ", ")
			if tags == "" {
				tags = "なし"
			}
			failed = append(failed, "失敗タグ（"+host+"）: "+tags)
		}
	}
	if len(versions) <= 1 {
		return []any{}
	}
	keys := []string{}
	for key := range versions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := []string{}
	for _, key := range keys {
		hosts := versions[key]
		sort.Strings(hosts)
		lines = append(lines, key+": "+strings.Join(hosts, ", "))
	}
	lines = append(lines, failed...)
	var since time.Time
	for _, hold := range drift.Drifts {
		if since.IsZero() || hold.Since.Before(since) {
			since = hold.Since
		}
	}
	lines = append(lines, "未反映の発行時刻: "+since.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04")+"（日本時間）", "古いイメージのPCを新しいイメージへ入れ替えてください。そろうと消えます。")
	return []any{map[string]any{"id": "host-drift-image", "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": nil, "detectedAt": iso(since), "title": "PCのイメージの版がそろっていません", "detail": strings.Join(lines, "\n")}}
}
