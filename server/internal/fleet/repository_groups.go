package fleet

import "strings"

func normalizeRepositoryGroups(raw map[string]any) ([][]string, error) {
	groups := make([][]string, 0)
	value, exists := raw["repositoryGroups"]
	if !exists {
		return groups, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, invalid()
	}
	seen := map[string]bool{}
	for _, item := range list {
		members, ok := item.([]any)
		if !ok || len(members) < 2 {
			return nil, invalid()
		}
		group := make([]string, 0, len(members))
		for _, member := range members {
			name, ok := member.(string)
			key := strings.ToLower(name)
			if !ok || !ValidRepo(name) || seen[key] {
				return nil, invalid()
			}
			seen[key] = true
			group = append(group, name)
		}
		groups = append(groups, group)
	}
	return groups, nil
}
