package projects

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
)

const assignmentsFile = "assignments.json"
const fleetFile = "fleet.json"
const registryFile = "deploy/nas/repositories.json"

type DeskFile struct{ Text, SHA string }
type DeskChange struct{ Path, Text, SHA string }

func parseObject(text string) (Object, error) {
	v := Object{}
	e := json.Unmarshal([]byte(strings.TrimPrefix(text, "\ufeff")), &v)
	return v, e
}
func span(text, key string) (int, int, error) {
	depth := 0
	last, current := "", ""
	start := -1
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '"':
			j := i + 1
			for j < len(text) {
				if text[j] == '\\' {
					j += 2
					continue
				}
				if text[j] == '"' {
					break
				}
				j++
			}
			if j >= len(text) {
				return 0, 0, fmt.Errorf("unterminated JSON string")
			}
			if depth == 1 {
				_ = json.Unmarshal([]byte(text[i:j+1]), &last)
			}
			i = j
		case ':':
			if depth == 1 {
				current = last
			}
		case ',':
			if depth == 1 {
				current = ""
			}
		case '{', '[':
			if depth == 1 && current == key && start < 0 {
				start = i
			}
			depth++
		case '}', ']':
			depth--
			if depth == 1 && start >= 0 {
				return start, i, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("JSON key %s not found", key)
}
func indent(text string, at int) string {
	line := text[strings.LastIndex(text[:at], "\n")+1 : at]
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i]
}
func appendNested(text string, keys []string, key string, value any) (string, error) {
	start, end, e := span(text, keys[0])
	if e != nil {
		return "", e
	}
	if len(keys) > 1 {
		inner, e := appendNested(text[start:end+1], keys[1:], key, value)
		if e != nil {
			return "", e
		}
		return text[:start] + inner + text[end+1:], nil
	}
	k := end - 1
	for k > start && strings.ContainsRune(" \t\r\n", rune(text[k])) {
		k--
	}
	prefix := ""
	if key != "" {
		prefix = string(jsonBytes(key)) + ": "
	}
	content := prefix + inlineJSON(value)
	if k == start {
		outer := indent(text, start)
		return text[:start+1] + "\n" + outer + "  " + content + "\n" + outer + text[end:], nil
	}
	return text[:k+1] + ",\n" + indent(text, k) + content + text[k+1:], nil
}
func inlineJSON(v any) string {
	switch value := v.(type) {
	case []any:
		items := []string{}
		for _, x := range value {
			items = append(items, inlineJSON(x))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case map[string]any:
		keys := []string{"name", "displayName", "description", "autoMerge", "autoDeploy", "testCommand", "setupCommand", "rank"}
		parts := []string{}
		for _, k := range keys {
			if value[k] != nil {
				parts = append(parts, string(jsonBytes(k))+": "+inlineJSON(value[k]))
			}
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	default:
		return string(jsonBytes(v))
	}
}

// PlanDeskChange adds only the selected host's exact repository entries.
// Existing entries with different spelling, placement or values are conflicts.
func PlanDeskChange(files map[string]DeskFile, entry Object, host string) ([]DeskChange, error) {
	full := fleet.Text(entry["name"])
	changes := []DeskChange{}
	change, e := planAssignmentChange(files, full, host)
	if e != nil {
		return nil, e
	}
	if change != nil {
		changes = append(changes, *change)
	}
	if change, e = planFleetChange(files, entry, full, host); e != nil {
		return nil, e
	}
	if change != nil {
		changes = append(changes, *change)
	}
	if host != "nas" {
		return changes, nil
	}
	if change, e = planRegistryChange(files, entry, full); e != nil {
		return nil, e
	}
	if change != nil {
		changes = append(changes, *change)
	}
	return changes, nil
}

// planAssignmentChange は担当表に配置先のPCの担当として追加する変更を作る。既に同じ設定があれば nil を返し、別の綴りや値の設定があれば止める。
func planAssignmentChange(files map[string]DeskFile, full, host string) (*DeskChange, error) {
	assignment, e := parseObject(files[assignmentsFile].Text)
	if e != nil {
		return nil, e
	}
	hosts := fleet.ObjectOrEmpty(assignment["hosts"])
	target := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(hosts[host])["repositories"])
	if fleet.ObjectOrEmpty(hosts[host])["repositories"] == nil {
		return nil, block("配置先の担当表がありません。")
	}
	found := false
	for pc, v := range hosts {
		for k, value := range fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(v)["repositories"]) {
			if strings.EqualFold(k, full) {
				if found || pc != host || k != full || !reflect.DeepEqual(value, Object{"rank": float64(1)}) {
					return nil, block("担当表に同じ名前の別の設定があります。")
				}
				found = true
			}
		}
	}
	for k := range fleet.ObjectOrEmpty(assignment["repositories"]) {
		if strings.EqualFold(k, full) {
			return nil, block("旧形式の担当表に同じ名前の設定があります。")
		}
	}
	if !found {
		text, e := appendNested(files[assignmentsFile].Text, []string{"hosts", host, "repositories"}, full, Object{"rank": 1})
		if e != nil {
			return nil, e
		}
		target[full] = Object{"rank": float64(1)}
		actual, e := parseObject(text)
		if e != nil || !reflect.DeepEqual(actual, assignment) {
			return nil, fmt.Errorf("assignment addition changed other fields")
		}
		return &DeskChange{assignmentsFile, text, files[assignmentsFile].SHA}, nil
	}
	return nil, nil
}

// planFleetChange は fleet の配置先のPCの登録一覧に追加する変更を作る。既に同じ設定があれば nil を返す。
func planFleetChange(files map[string]DeskFile, entry Object, full, host string) (*DeskChange, error) {
	fleetConfig, e := parseObject(files[fleetFile].Text)
	if e != nil {
		return nil, e
	}
	hostConfig := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleetConfig["hosts"])[host])
	if _, ok := hostConfig["repositories"].([]any); !ok {
		return nil, block("配置先のfleet登録一覧がありません。")
	}
	want := clone(entry)
	repos := fleet.List(hostConfig["repositories"])
	exists := false
	for _, v := range repos {
		m := fleet.ObjectOrEmpty(v)
		if strings.EqualFold(fleet.Text(m["name"]), full) {
			if exists || !reflect.DeepEqual(m, want) {
				return nil, block("fleetに同じ名前の別の設定があります。")
			}
			exists = true
		}
	}
	if !exists {
		text, e := appendNested(files[fleetFile].Text, []string{"hosts", host, "repositories"}, "", want)
		if e != nil {
			return nil, e
		}
		hostConfig["repositories"] = append(repos, want)
		actual, e := parseObject(text)
		if e != nil || !reflect.DeepEqual(actual, fleetConfig) {
			return nil, fmt.Errorf("fleet addition changed other fields")
		}
		return &DeskChange{fleetFile, text, files[fleetFile].SHA}, nil
	}
	return nil, nil
}

// planRegistryChange はNASの登録一覧に追加する変更を作る。既に同じ設定があれば nil を返す。
func planRegistryChange(files map[string]DeskFile, entry Object, full string) (*DeskChange, error) {
	want := clone(entry)
	delete(want, "setupCommand")
	want["autoDeploy"] = false
	source := files[registryFile]
	if source.Text == "" {
		source.Text = "{\n  \"repositories\": []\n}\n"
	}
	registry, e := parseObject(source.Text)
	if e != nil {
		return nil, e
	}
	if _, ok := registry["repositories"].([]any); !ok {
		return nil, fmt.Errorf("invalid registry")
	}
	exists := false
	for _, v := range fleet.List(registry["repositories"]) {
		m := fleet.ObjectOrEmpty(v)
		if strings.EqualFold(fleet.Text(m["name"]), full) {
			if exists || !reflect.DeepEqual(m, want) {
				return nil, block("NAS登録一覧に同じ名前の別の設定があります。")
			}
			exists = true
		}
	}
	if !exists {
		text, e := appendNested(source.Text, []string{"repositories"}, "", want)
		if e != nil {
			return nil, e
		}
		registry["repositories"] = append(fleet.List(registry["repositories"]), want)
		actual, e := parseObject(text)
		if e != nil || !reflect.DeepEqual(actual, registry) {
			return nil, fmt.Errorf("registry addition changed other fields")
		}
		return &DeskChange{registryFile, text, source.SHA}, nil
	}
	return nil, nil
}
