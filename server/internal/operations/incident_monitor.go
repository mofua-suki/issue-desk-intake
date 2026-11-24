package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/notices"
	"sort"
	"sync"
	"time"
)

type IncidentSource struct {
	Key, Kind, Title string
	Target           any
}
type IncidentMonitor struct {
	Service *Service
	Sources func(context.Context) ([]IncidentSource, error)
	Notify  func(context.Context, map[string]any) (map[string]any, error)
	Actions *IncidentActions
	mu      sync.Mutex
	last    time.Time
}

func (m *IncidentMonitor) Tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.Service.now()
	if !m.last.IsZero() && at.Sub(m.last) >= 0 && at.Sub(m.last) < time.Minute {
		return nil
	}
	m.last = at
	return m.Reconcile(ctx)
}
func (m *IncidentMonitor) Reconcile(ctx context.Context) error {
	s := m.Service
	now := s.now()
	sources, e := m.Sources(ctx)
	if e != nil {
		return e
	}
	current := map[string]IncidentSource{}
	for _, source := range sources {
		current[source.Key] = source
	}
	// Read outside the ledger transaction so a saturated pool cannot deadlock.
	slowActive, e := s.activeSlowIncidentKeys(ctx)
	if e != nil {
		return e
	}
	added, expired := []string{}, []string{}
	e = s.mutateIncidents(ctx, func(list *[]any) error {
		rows := *list
		newlyExpired, err := s.closeOrExpireIncidents(rows, current, now)
		if err != nil {
			return err
		}
		expired = append(expired, newlyExpired...)
		s.acknowledgeEndedIncidents(rows, now)
		for _, source := range sources {
			exists := false
			for _, raw := range rows {
				r := fleet.Object(raw)
				if r["key"] == source.Key && (r["endedAt"] == nil && r["state"] != "resolved" || source.Kind == "slow" && r["slowJudgement"] == "no_problem") {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			if len(rows) >= 200 {
				next, evicted := evictIncident(rows, now, slowActive)
				if !evicted {
					break
				}
				rows = next
			}
			id, e := requestID()
			if e != nil {
				return e
			}
			r := map[string]any{"id": id, "key": source.Key, "kind": source.Kind, "title": source.Title, "target": source.Target, "detectedAt": iso(now), "deadline": iso(now.Add(15 * time.Minute)), "state": "waiting", "history": []any{}}
			s.incidentNote(r, "検知", incidentKindName(source.Kind))
			s.incidentNote(r, "通知開始", "送信結果が無い場合は再起動などで結果を確認できません。再送しません。")
			rows = append(rows, r)
			added = append(added, id)
		}
		*list = rows
		return nil
	})
	if e != nil {
		return e
	}
	for _, id := range added {
		if e = m.notifyIncident(ctx, id); e != nil {
			return e
		}
	}
	for _, id := range expired {
		if m.Actions == nil {
			continue
		}
		value, err := s.OperationalSetting(ctx, "incident-deadline")
		if err != nil {
			return err
		}
		if row := incidentRow(fleet.List(value), id); row != nil && row["kind"] == "slow" {
			continue
		}
		if e = m.Actions.Execute(ctx, id); e != nil {
			return e
		}
	}
	return nil
}

// activeSlowIncidentKeys は記録が上限に近く「問題なし」と判断した遅延の有事があるとき、まだ続いている遅延作業の鍵を読む。それ以外は nil を返す。
func (s *Service) activeSlowIncidentKeys(ctx context.Context) (map[string]bool, error) {
	var slowActive map[string]bool
	saved, e := s.OperationalSetting(ctx, "incident-deadline")
	if e != nil {
		return nil, e
	}
	if len(fleet.List(saved)) >= 200 {
		for _, raw := range fleet.List(saved) {
			if fleet.Object(raw)["slowJudgement"] == "no_problem" {
				work, err := s.slowWorkRows(ctx)
				if err != nil {
					return nil, err
				}
				slowActive = map[string]bool{}
				for _, w := range work {
					if w.end == nil {
						slowActive["slow-"+w.key] = true
					}
				}
				break
			}
		}
	}
	return slowActive, nil
}

// closeOrExpireIncidents は要対応から消えた有事を終え、回答待ちのまま期限を過ぎた有事を期限切れにし、対処を始める有事の番号を返す。
func (s *Service) closeOrExpireIncidents(rows []any, current map[string]IncidentSource, now time.Time) ([]string, error) {
	expired := []string{}
	for _, raw := range rows {
		r := fleet.Object(raw)
		if r["endedAt"] != nil {
			continue
		}
		_, present := current[fleet.Text(r["key"])]
		if !present {
			r["endedAt"] = iso(now)
			if r["state"] == "waiting" {
				r["state"] = "resolved"
				s.incidentNote(r, "解消", "要対応から消えました。")
			} else {
				s.incidentNote(r, "有事終了", "要対応から消えました。対応・確認の状態は保持します。")
			}
		} else if r["state"] == "waiting" {
			deadline, err := time.Parse(time.RFC3339Nano, fleet.Text(r["deadline"]))
			if err != nil {
				return nil, fmt.Errorf("有事の期限を読めません")
			}
			if !now.Before(deadline) {
				r["state"] = "expired"
				s.incidentNote(r, "期限切れ", "期限までに「自分で対応する」の返答がありませんでした。")
				expired = append(expired, fleet.Text(r["id"]))
			}
		} else if r["state"] == "expired" && r["actionStartedAt"] == nil && r["actionInvokedAt"] == nil {
			expired = append(expired, fleet.Text(r["id"]))
		}
	}
	return expired, nil
}

// acknowledgeEndedIncidents は解消・対処済みで利用者の確認が要らない有事を、自動で確認済みにする。
func (s *Service) acknowledgeEndedIncidents(rows []any, now time.Time) {
	for _, raw := range rows {
		r := fleet.Object(raw)
		ended := r["endedAt"] != nil || r["state"] == "resolved"
		if r["acknowledgedAt"] == nil && ended && !slowFilingNeedsAttention(r) && (r["state"] == "resolved" || r["state"] == "acted" || r["kind"] == "slow") {
			r["acknowledgedAt"] = iso(now)
			reason := "元の要対応が消えた解消／対処済みのため要対応から外した"
			if r["kind"] == "slow" {
				reason = "遅延作業が発生源から外れたため要対応から外した"
			}
			s.incidentNote(r, "自動の確認済み", reason)
		}
	}
}

// evictIncident は記録が上限に達したとき、確認済みで終わった有事のうち最も古く確認したものを1件外す。外せるものが無ければ false を返す。
func evictIncident(rows []any, now time.Time, slowActive map[string]bool) ([]any, bool) {
	candidates := []map[string]any{}
	day := now.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02")
	for _, raw := range rows {
		r := fleet.Object(raw)
		if r["slowJudgement"] == "no_problem" {
			if slowActive == nil || slowActive[fleet.Text(r["key"])] {
				continue
			}
		}
		if r["acknowledgedAt"] == nil || r["endedAt"] == nil && r["state"] != "resolved" {
			continue
		}
		eligible := true
		for _, raw := range fleet.List(r["actions"]) {
			a := fleet.Object(raw)
			if a["action"] == "file" && fleet.Text(a["day"]) >= day {
				eligible = false
			}
		}
		if eligible {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return fleet.Text(candidates[i]["acknowledgedAt"]) < fleet.Text(candidates[j]["acknowledgedAt"])
	})
	remove := candidates[0]["id"]
	next := []any{}
	for _, raw := range rows {
		if fleet.Object(raw)["id"] != remove {
			next = append(next, raw)
		}
	}
	return next, true
}

// notifyIncident は新しい有事を購読先へ知らせ、送信の結果を有事の記録に残す。
func (m *IncidentMonitor) notifyIncident(ctx context.Context, id string) error {
	s := m.Service
	var row map[string]any
	value, e := s.OperationalSetting(ctx, "incident-deadline")
	if e != nil {
		return e
	}
	row = incidentRow(fleet.List(value), id)
	result := map[string]any{"sent": 0, "failed": 0, "subscriptions": 0}
	if m.Notify != nil {
		deadline, _ := time.Parse(time.RFC3339Nano, fleet.Text(row["deadline"]))
		result, e = m.Notify(ctx, map[string]any{"title": "有事: 対応期限 " + deadline.In(time.FixedZone("JST", 9*3600)).Format("15:04"), "body": incidentKindName(fleet.Text(row["kind"])), "url": "/#/list/attention"})
		if e != nil {
			result = map[string]any{"failed": 1}
		}
	}
	reason := "購読がありません。期限は続きます。"
	if fleet.Number(result["failed"]) > 0 {
		reason = "送信に失敗しました。期限は続きます。"
	} else if fleet.Number(result["subscriptions"]) > 0 {
		reason = fmt.Sprintf("%v端末へ送信しました。", result["sent"])
	}
	return s.changeIncident(ctx, id, func(r map[string]any) error { s.incidentNote(r, "通知結果", reason); return nil })
}

func incidentKindName(kind string) string {
	return map[string]string{"slow": "作業の遅延", "stall": "止まり", "monitor_stopped": "ホームの監視の停止", "full_test": "main の全部のテストの失敗", "main_broken": "main の共通の失敗", "stage_watch": "段取り運用の異常結果"}[kind]
}
func (s *Service) IncidentSources(ctx context.Context) ([]IncidentSource, error) {
	out := []IncidentSource{}
	home, e := s.homeMonitorAlerts(ctx)
	if e != nil {
		return nil, e
	}
	for _, raw := range home {
		r := fleet.Object(raw)
		out = append(out, IncidentSource{Key: "stall-" + fleet.Text(r["id"]), Kind: "monitor_stopped", Title: fleet.Text(r["title"]), Target: map[string]any{"project": r["project"], "number": nil}})
	}
	full, e := s.FullTests(ctx)
	if e != nil {
		return nil, e
	}
	for _, raw := range full {
		r := fleet.Object(raw)
		target := r
		if f := fleet.Object(r["fix"]); f != nil {
			target = f
		}
		out = append(out, IncidentSource{Key: fleet.Text(r["id"]), Kind: "full_test", Title: fleet.Text(r["title"]), Target: incidentTarget(target)})
	}
	configured := map[string]bool{}
	if s.Repositories != nil {
		repos, e := s.Repositories()
		if e != nil {
			return nil, e
		}
		for _, repo := range repos {
			configured[repo] = true
		}
	}
	saved, e := s.savedAttention(ctx, configured)
	if e != nil {
		return nil, e
	}
	for _, raw := range saved {
		r := fleet.Object(raw)
		if r["kind"] == "main_broken" {
			target := r
			if inv := fleet.Object(r["investigation"]); inv != nil {
				target = inv
			}
			out = append(out, IncidentSource{Key: fleet.Text(r["key"]), Kind: "main_broken", Title: fleet.Text(r["title"]), Target: incidentTarget(target)})
		}
	}
	stage, e := (&notices.Service{DB: s.DB, Now: s.Now, Repositories: s.Repositories}).StageAlerts(ctx)
	if e != nil {
		return nil, e
	}
	for _, raw := range stage {
		r := fleet.Object(raw)
		if r["event"] == "result" && (r["exitCode"] != float64(0) || r["outputs"] == float64(0) || len(fleet.List(r["anomalies"])) > 0) {
			out = append(out, IncidentSource{Key: fleet.Text(r["key"]), Kind: "stage_watch", Title: fleet.Text(r["title"]), Target: map[string]any{"project": r["project"], "number": r["number"]}})
		}
	}
	return out, nil
}

func incidentTarget(row map[string]any) map[string]any {
	project := row["project"]
	if project == nil || project == "" {
		project = row["repo"]
	}
	if project == nil {
		project = ""
	}
	number := row["number"]
	if number == nil {
		if issue := fleet.Object(row["issue"]); issue != nil {
			number = issue["number"]
		} else {
			number = row["issue"]
		}
	}
	return map[string]any{"project": project, "number": number}
}
