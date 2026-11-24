package httpapi

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func requestedRepositories(body map[string]any) ([]string, bool, error) {
	raw, present := body["repos"]
	if !present {
		return nil, false, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > 1000 {
		return nil, true, contract.NewProblem(400, "reposは1000件以内のリポジトリ名の配列です。", "")
	}
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		repo, ok := value.(string)
		if !ok || !fleet.ValidRepo(repo) {
			return nil, true, contract.NewProblem(400, "reposのリポジトリ名が不正です。", "")
		}
		key := strings.ToLower(repo)
		if !seen[key] {
			seen[key] = true
			result = append(result, repo)
		}
	}
	return result, true, nil
}

// Both restrictions only narrow the administrator's assignment in jobs.Next.
// An explicitly empty list admits no repositories.
func dispatchRestriction(requested []string, provided bool, drift []string) ([]string, bool) {
	if !provided {
		return drift, len(drift) > 0
	}
	if len(drift) == 0 {
		return requested, true
	}
	allowed := map[string]bool{}
	for _, repo := range drift {
		allowed[strings.ToLower(repo)] = true
	}
	result := []string{}
	for _, repo := range requested {
		if allowed[strings.ToLower(repo)] {
			result = append(result, repo)
		}
	}
	return result, true
}
