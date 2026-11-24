package fleet

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
)

func (s *Service) Targets() (map[string]string, map[string]map[string]any, error) {
	document, a, err := s.Load()
	if err != nil {
		return nil, nil, err
	}
	deploy := map[string]string{}
	live := map[string]map[string]any{}
	for host, config := range document.Hosts {
		merged := map[string]any{}
		for k, v := range document.Defaults {
			merged[k] = v
		}
		for k, v := range config {
			merged[k] = v
		}
		for _, raw := range List(merged["repositories"]) {
			repo := Object(raw)
			name := Text(repo["name"])
			if !SelfDeploymentRepository(name) && repo["autoDeploy"] == true && a.DeployHosts[name] == host {
				deploy[name] = host
			}
			if value, exists := repo["liveCheck"]; exists {
				v := Object(value)
				if !validLiveConfig(v) || document.Hosts[Text(v["host"])] == nil {
					return nil, nil, errors.New("実地確認の設定が不正です")
				}
				if old := live[name]; old != nil && !reflect.DeepEqual(old, v) {
					return nil, nil, errors.New("実地確認の設定がホスト間で異なります")
				}
				live[name] = v
			}
		}
	}
	return deploy, live, nil
}
func validLiveConfig(v map[string]any) bool {
	if len(v) != 3 || !known(v, "command", "host", "timeoutSeconds") || !ValidHost(Text(v["host"])) || !integer(v["timeoutSeconds"], 1, 3600) {
		return false
	}
	command, ok := v["command"].([]any)
	if !ok || len(command) == 0 || len(command) > 64 {
		return false
	}
	unsafe := regexp.MustCompile(`(?i)[\x00-\x1f\x7f-\x9f]|data:|base64|iVBORw0KGgo|/9j/|R0lGOD|<img|\b(?:bytes|thumbnail|password|authorization|credential|secret|token)\b|\b(?:idk_|idh_|sk-)|https?:|[?&][^\s=]+\s*=|[A-Za-z0-9+/=_-]{32,}|\\x[0-9a-f]{2}|\x{FFFD}`)
	for _, raw := range command {
		value, ok := raw.(string)
		if !ok || value == "" || len(value) > 1000 || strings.ContainsAny(value, "\r\n\x00") || unsafe.MatchString(value) {
			return false
		}
	}
	return true
}

// SelfDeploymentRepository identifies repositories updated through host/container release paths.
func SelfDeploymentRepository(repo string) bool {
	switch strings.ToLower(repo) {
	case "mofua-suki/issue-desk-worker", "mofua-suki/issue-desk-intake":
		return true
	}
	return false
}
