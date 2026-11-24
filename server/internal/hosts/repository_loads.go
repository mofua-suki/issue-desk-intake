package hosts

import (
	"context"
	"encoding/json"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

type repositoryCount struct{ implementing, publishing int }
type repositoryKey struct{ host, repo string }

func (s *Service) repositoryLoads(ctx context.Context, now time.Time) (map[repositoryKey]repositoryCount, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT lease_host,repo,view FROM jobs WHERE status='running' AND lease_host IS NOT NULL AND lease_expires_at>$1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[repositoryKey]repositoryCount{}
	for rows.Next() {
		var key repositoryKey
		var raw []byte
		var view map[string]any
		if err = rows.Scan(&key.host, &key.repo, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &view); err != nil {
			return nil, err
		}
		count := counts[key]
		if fleet.PublicationPhase(view) {
			count.publishing++
		} else {
			count.implementing++
		}
		counts[key] = count
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func assignmentLoads(a *fleet.Assignments, host string, counts map[repositoryKey]repositoryCount, err error) any {
	if a.Hosts[host] == nil {
		return nil
	}
	result := []any{}
	for _, repo := range a.Repositories(host) {
		row := map[string]any{"repository": repo, "max": a.RepoLimit(host, repo), "implementing": nil, "publishing": nil}
		if err == nil {
			count := counts[repositoryKey{host, repo}]
			row["implementing"] = count.implementing
			row["publishing"] = count.publishing
		}
		result = append(result, row)
	}
	return result
}

// hostLoad は担当表にないリポジトリを含むPC全体の有効lease数を返す。
func hostLoad(host string, counts map[repositoryKey]repositoryCount, err error) any {
	if err != nil {
		return nil
	}
	total := repositoryCount{}
	for key, count := range counts {
		if key.host == host {
			total.implementing += count.implementing
			total.publishing += count.publishing
		}
	}
	return map[string]any{"implementing": total.implementing, "publishing": total.publishing}
}
