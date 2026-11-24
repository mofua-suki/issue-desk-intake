package adminapi

import (
	"regexp"
	"strings"
)

// idRoute splits a path below a resource prefix, [/<id>[/<action>]], into the
// id and the action. An empty path is the collection. An id that does not
// match pattern, or more than two segments, answers 404 with notFound.
func idRoute(path string, pattern *regexp.Regexp, notFound string) (id, action string, _ error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if path != "" {
		id = parts[0]
		if !pattern.MatchString(id) || len(parts) > 2 {
			return "", "", bad(404, notFound)
		}
		if len(parts) == 2 {
			action = parts[1]
		}
	}
	return id, action, nil
}
