// 配布する設定の検査（資格情報の混入、既知のキー、環境の値）。

package fleet

import (
	"errors"
	"strings"
)

func CheckSecrets(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if secretKeyPattern.MatchString(key) && !strings.HasSuffix(key, "File") && !strings.HasSuffix(key, "Sha256") {
				return errors.New("fleetに資格情報の値は保存できません")
			}
			if err := CheckSecrets(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := CheckSecrets(child); err != nil {
				return err
			}
		}
	case string:
		if secretValuePattern.MatchString(v) {
			return errors.New("fleetに資格情報の値は保存できません")
		}
	}
	return nil
}

var configKeys = strings.Fields("giteaUrl publicUrl gitUser allowedUsers repositories jobTimeoutSeconds testTimeoutSeconds runtimeDir tokenFile workerTokenFile intakeAccount webhook codex claude node pnpm git python pythonFormatter codexSandboxDir model claudeModel codexEquivalents reasoningEffort provider implementation review changeSize usageWeights antigravity priority stallWatch opsWatch workerPlacement cliVersionWatch rateLimits implementationContext environment")

func checkConfig(config map[string]any) error {
	if config == nil {
		return errors.New("fleet設定はオブジェクトで指定してください")
	}
	delete(config, "emergency")
	for key := range config {
		allowed := false
		for _, candidate := range configKeys {
			if key == candidate {
				allowed = true
			}
		}
		if !allowed {
			return errors.New("fleetに未対応の設定項目があります")
		}
	}
	if raw, exists := config["environment"]; exists {
		env, ok := raw.(map[string]any)
		if !ok || env == nil {
			return errors.New("fleetのenvironmentはオブジェクトで指定してください")
		}
		for key, value := range env {
			if value == nil {
				switch key {
				case "platform", "gcc", "cgo", "race", "docker", "realDevice", "liveMeasurement", "network":
					continue
				}
			}
			switch key {
			case "gcc", "cgo", "race", "docker", "realDevice", "liveMeasurement":
				if _, ok := value.(bool); !ok {
					return errors.New("fleetのenvironmentの能力値が不正です")
				}
			case "platform":
				if v, ok := value.(string); !ok || (v != "linux" && v != "darwin") {
					return ErrPlatform
				}
			case "network":
				if v, ok := value.(string); !ok || (v != "allowlist" && v != "none") {
					return errors.New("fleetのenvironmentのnetworkが不正です")
				}
			default:
				return errors.New("fleetのenvironmentに未対応の項目があります")
			}
		}
	}
	return CheckSecrets(config)
}
