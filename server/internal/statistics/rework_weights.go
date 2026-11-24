package statistics

import (
	"fmt"
)

func validateBreakdownWeights(value AccountRecord) error {
	checkKeys := func(m AccountRecord, keys []string) error {
		for key := range m {
			if !includes(keys, key) {
				return fmt.Errorf("使用量の重み設定に不明な項目があります")
			}
		}
		return nil
	}
	weights := func(value any) error {
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("使用量の重み設定はオブジェクトです")
		}
		for key, v := range m {
			if !includes([]string{"input", "cacheRead", "cacheWrite", "output"}, key) {
				return fmt.Errorf("使用量の重みの項目が不正です")
			}
			if _, ok := accountKnown(v); !ok {
				return fmt.Errorf("使用量の重みは0以上の有限数です")
			}
		}
		return nil
	}
	if e := checkKeys(value, []string{"default", "providers"}); e != nil {
		return e
	}
	if raw, has := value["default"]; has {
		if e := weights(raw); e != nil {
			return e
		}
	}
	if raw, has := value["providers"]; has {
		providers, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("実行先ごとの重み設定が不正です")
		}
		for _, raw := range providers {
			entry, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("実行先の重み設定が不正です")
			}
			if e := checkKeys(entry, []string{"default", "models"}); e != nil {
				return e
			}
			if raw, has := entry["default"]; has {
				if e := weights(raw); e != nil {
					return e
				}
			}
			if raw, has := entry["models"]; has {
				models, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("モデルごとの重み設定が不正です")
				}
				for _, raw := range models {
					if e := weights(raw); e != nil {
						return e
					}
				}
			}
		}
	}
	return nil
}
