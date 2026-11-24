package hostdrift

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
)

func HoldWording(stopping bool) (string, string) {
	if stopping {
		return "版がそろうまで配れません", ""
	}
	return "版ずれですが配布は続けています", "（配布は続けています）"
}
func (s *Service) Decorate(ctx context.Context, a *fleet.Assignments, view map[string]any) error {
	snapshot, e := s.Evaluate(ctx, a)
	if e != nil {
		return e
	}
	for _, raw := range fleet.List(view["hosts"]) {
		r := fleet.Object(raw)
		host := fleet.Text(r["name"])
		hold, has := snapshot.Drifts[host]
		if !has {
			r["driftHold"] = nil
			r["drift"] = nil
			continue
		}
		_, stopping := snapshot.Holds[host]
		title, _ := HoldWording(stopping)
		repos := append([]string{}, snapshot.Allowed[host]...)
		sort.Strings(repos)
		var behind any
		if hold.Behind != nil {
			behind = *hold.Behind
		}
		detail := map[string]any{"current": hold.Revision, "desired": hold.Desired, "kind": hold.Kind, "stopping": stopping, "title": title, "since": iso(hold.Since), "revision": hold.Revision, "desiredRevision": hold.Desired, "behind": behind, "includesAlign": hold.IncludesAlign, "allowedRepositories": repos}
		r["drift"] = detail
		if stopping {
			r["driftHold"] = detail
		} else {
			r["driftHold"] = nil
		}
	}
	return nil
}
func decorateAlerts(out *Snapshot) {
	for _, raw := range out.Alerts {
		r := fleet.Object(raw)
		host := fleet.Text(r["host"])
		if fleet.Text(r["id"]) != "host-drift-revision-"+host {
			continue
		}
		_, stopping := out.Holds[host]
		title, suffix := HoldWording(stopping)
		r["title"] = fleet.Text(r["title"]) + suffix
		r["detail"] = fmt.Sprintf("%v\n配布の状態: %s", r["detail"], title)
		if repos := out.Allowed[host]; len(repos) > 0 {
			ordered := append([]string{}, repos...)
			sort.Strings(ordered)
			r["detail"] = fleet.Text(r["detail"]) + "\n救済で配布を続けるリポジトリ: " + strings.Join(ordered, ", ")
		}
	}
}
