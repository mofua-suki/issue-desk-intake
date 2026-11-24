package fleet

import (
	"errors"
	"path/filepath"
)

// EffectiveConfig returns the same host overrides used by ClientConfig.
func EffectiveConfig(document Document, host string) map[string]any {
	config := Object(Clone(document.Defaults))
	for key, value := range document.Hosts[host] {
		config[key] = Clone(value)
	}
	return config
}

func RepositoryConfig(document Document, host, repo string) map[string]any {
	for _, value := range List(EffectiveConfig(document, host)["repositories"]) {
		if entry := Object(value); Text(entry["name"]) == repo {
			return entry
		}
	}
	return nil
}

func (a *Assignments) CanTest(host, repo string) bool {
	entry := a.Hosts[host]
	return entry != nil && contains(entry.TestRepositories, repo)
}

func (a *Assignments) CanImplement(host, repo string) bool {
	return a.Hosts[host] != nil && a.Hosts[host].Role != "test-only" && contains(a.Implementers[repo], host)
}

func NativeTestTarget(document Document, assignments *Assignments, sender, repo string) string {
	if !assignments.CanImplement(sender, repo) {
		return ""
	}
	return Text(RepositoryConfig(document, sender, repo)["nativeTestHost"])
}

func validateNativeTests(document Document, assignments *Assignments) error {
	for host, entry := range assignments.Hosts {
		config := EffectiveConfig(document, host)
		if Text(Object(config["environment"])["platform"]) == "darwin" && len(entry.Repositories) != 0 {
			return errors.New("MacネイティブのホストIDはテスト用です。実装担当はLinuxコンテナ用の別IDにしてください")
		}
		for _, repo := range entry.TestRepositories {
			repository := RepositoryConfig(document, host, repo)
			if value, exists := repository["nativeNetwork"]; exists {
				if _, ok := value.(bool); !ok {
					return errors.New("nativeNetworkは真偽値です")
				}
			}
			if value, exists := repository["nativeToolRoots"]; exists {
				roots := Strings(value)
				if len(roots) == 0 || len(roots) > 8 || len(roots) != len(List(value)) {
					return errors.New("nativeToolRootsは絶対パスの配列です")
				}
				for _, root := range roots {
					if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
						return errors.New("nativeToolRootsにルート全体は指定できません")
					}
				}
			}
			command := Strings(repository["testCommand"])
			if document.Hosts[host] == nil || Text(Object(config["environment"])["platform"]) != "darwin" || len(command) == 0 || len(command) != len(List(repository["testCommand"])) {
				return errors.New("Macテスト担当にはdarwinの環境と固定testCommandが必要です")
			}
			for _, arg := range command {
				if arg == "" {
					return errors.New("Macテストコマンドは空にできません")
				}
			}
		}
		for _, value := range List(config["repositories"]) {
			repository := Object(value)
			if raw, exists := repository["nativeTestHost"]; exists {
				target := Text(raw)
				if !ValidHost(target) || !assignments.CanTest(target, Text(repository["name"])) {
					return errors.New("nativeTestHostはそのリポジトリのMacテスト担当を指定してください")
				}
			}
		}
	}
	// Every implementer must enforce the same gate; a lower ranked PC cannot bypass it.
	for repo, hosts := range assignments.Implementers {
		target := ""
		for i, host := range hosts {
			value := Text(RepositoryConfig(document, host, repo)["nativeTestHost"])
			if i == 0 {
				target = value
			} else if value != target {
				return errors.New("同じリポジトリの全実装担当に同じnativeTestHostを設定してください")
			}
		}
	}
	return nil
}
