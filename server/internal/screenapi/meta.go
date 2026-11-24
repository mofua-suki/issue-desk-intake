// 画面の付帯情報（meta: プロジェクト・PC・AIの状態・監視の稼働・プロジェクト作成の設定）を組み立てる。

package screenapi

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"time"
)

func (s *Service) meta(ctx context.Context, origin string) (Object, error) {
	if s.o.Meta != nil {
		return s.o.Meta(ctx)
	}
	c, e := s.o.Config()
	if e != nil {
		return nil, e
	}
	if s.o.Providers == nil {
		return nil, fail(503, "利用可能なAIの状態を取得できません。")
	}
	providers, e := s.o.Providers(ctx)
	if e != nil {
		return nil, e
	}
	projects := []any{}
	for _, v := range fleet.Items(c["repositories"]) {
		r := fleet.ObjectOrEmpty(v)
		name := fleet.Text(r["displayName"])
		if name == "" {
			name = fleet.Text(r["name"])
		}
		host := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(c["assignments"])["repositories"])[fleet.Text(r["name"])]
		projects = append(projects, Object{"id": r["name"], "name": name, "description": fleet.Text(r["description"]), "host": host})
	}
	hosts := []string{}
	for _, value := range fleet.Items(fleet.ObjectOrEmpty(c["assignments"])["hosts"]) {
		if host, ok := value.(string); ok {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	if s.o.Postgres == nil {
		return nil, fail(503, "監視の稼働状態を取得できません。")
	}
	var beat time.Time
	e = s.o.Postgres.QueryRowContext(ctx, `SELECT heartbeat_at FROM hosts WHERE host=$1`, fleet.Text(c["workerId"])).Scan(&beat)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	worker := e == nil && s.o.Now().Sub(beat) <= 15*time.Minute
	out := Object{"projects": projects, "workerId": c["workerId"], "hosts": hosts, "providers": providers, "worker": worker, "user": c["gitUser"], "giteaAccount": c["giteaAccount"], "publicUrl": origin}
	if s.o.RuntimeMeasurements != nil {
		out["runtimeMeasurements"] = s.o.RuntimeMeasurements()
	}
	if creation := fleet.ObjectOrEmpty(c["projectCreation"]); regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(fleet.Text(creation["owner"])) {
		out["projectCreation"] = Object{"owner": creation["owner"], "technologies": []string{"node", "python"}, "placements": []string{"a9max", "nas"}}
	}
	return out, nil
}
