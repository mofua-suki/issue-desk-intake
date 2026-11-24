package fleet

import (
	"errors"
	"golang.org/x/text/unicode/norm"
	"issue-desk-intake/server/internal/contract"
	"regexp"
	"strings"
	"unicode"
)

var reservedScopePart = regexp.MustCompile(`^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[0-9¹²³]|LPT[0-9¹²³])$`)

func exemptFiles(value any) ([]any, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 200 {
		return nil, errors.New("範囲の除外は完全一致のファイル200件までです")
	}
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range list {
		input, ok := raw.(string)
		if !ok {
			return nil, invalid()
		}
		value := strings.ReplaceAll(strings.TrimSpace(norm.NFC.String(input)), "\\", "/")
		if value == "" || contract.UTF16Length(value) > 200 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
			return nil, invalid()
		}
		for _, r := range value {
			if r < 32 || r == 127 || strings.ContainsRune("*?[]{}<>|\"`:／＼", r) {
				return nil, invalid()
			}
		}
		parts := []string{}
		for _, part := range strings.Split(value, "/") {
			if part == "" || part == "." {
				continue
			}
			upper := strings.Map(unicode.ToUpper, part)
			base := strings.TrimRight(strings.Split(upper, ".")[0], " ")
			if part == ".." || upper == ".GIT" || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") || reservedScopePart.MatchString(base) {
				return nil, invalid()
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			return nil, invalid()
		}
		file := strings.Join(parts, "/")
		if !seen[file] {
			out = append(out, file)
			seen[file] = true
		}
	}
	return out, nil
}
func validateExemptions(dispatch map[string]any) error {
	if value, exists := dispatch["scopeExemptPaths"]; exists {
		files, e := exemptFiles(value)
		if e != nil {
			return e
		}
		dispatch["scopeExemptPaths"] = files
	}
	if value, exists := dispatch["scopeExemptRepositories"]; exists {
		repos := Object(value)
		if repos == nil {
			return invalid()
		}
		out := map[string]any{}
		for repo, value := range repos {
			if !ValidRepo(repo) {
				return invalid()
			}
			files, e := exemptFiles(value)
			if e != nil {
				return e
			}
			out[repo] = files
		}
		dispatch["scopeExemptRepositories"] = out
	}
	return nil
}
