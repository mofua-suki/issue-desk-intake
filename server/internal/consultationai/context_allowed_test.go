package consultationai

import "testing"

func TestContextAllowedReadsSourcePackagesNamedModelsButNotWeightsOrRootModels(t *testing.T) {
	for name, want := range map[string]bool{
		// ソースコードのパッケージ名としての models は読める（共通番号4490の確認に必要）
		"server/internal/models/resolve.go":     true,
		"server/internal/models/models_test.go": true,
		"src/models/user.py":                    true,
		"docs/models.md":                        true,
		"assignments.json":                      true,
		"skills/user-interaction/SKILL.md":      true,
		// リポジトリ直下の models/ は読ませない
		"models/sd15.safetensors": false,
		"models/readme.md":        false,
		"Models/config.json":      false,
		// 重みのファイルは、どの階層でも読ませない
		"server/internal/models/weights.safetensors": false,
		"src/models/checkpoint.ckpt":                 false,
		"src/models/llama.gguf":                      false,
		"src/models/net.pth":                         false,
		"src/models/arrays.npz":                      false,
		"src/models/net.h5":                          false,
		// 従来の拒否は変えない
		"data/raw.csv":              false,
		"server/runtime/state.json": false,
		"private/key.txt":           false,
		"images/a.png":              false,
		"node_modules/x/index.js":   false,
		"config.local.json":         false,
		"deploy/secrets/token.txt":  false,
		".env":                      false,
		".env.example":              true,
		"../escape":                 false,
		"/abs/path":                 false,
		"a/b/id_ed25519":            false,
	} {
		if got := contextAllowed(name); got != want {
			t.Errorf("contextAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestContextAllowedRuntimeSourceAndStateBoundary(t *testing.T) {
	for name, want := range map[string]bool{
		"internal/runtime/scheduler.go":            true,
		"internal/runtime/x.go":                    true,
		"server/internal/runtime/x_test.go":        true,
		"runtime/x":                                false,
		"server/runtime/state.json":                false,
		"deploy/nas/runtime/state/intake.json":     false,
		"deploy/nas/runtime/db-backups/backup.sql": false,
		"deploy/nas/runtime/x.go":                  false,
		"internal/runtime/state/x.go":              false,
		"internal/runtime/state.json":              false,
		"runtime/internal/runtime/x.go":            false,
		"internal/runtime/token.go":                false,
		"internal/runtime/.hidden.go":              false,
		"internal/runtime/x.go.bin":                false,
		"/internal/runtime/x.go":                   false,
		"internal/runtime/../x.go":                 false,
		"private/internal/runtime/x.go":            false,
		"server/internal/images/inspect.go":        false,
		"internal/data/x.go":                       false,
		"internal/downloads/x.go":                  false,
		"internal/profiles/x.go":                   false,
		"internal/secrets/x.go":                    false,
	} {
		if got := contextAllowed(name); got != want {
			t.Errorf("contextAllowed(%q) = %v, want %v", name, got, want)
		}
	}
}
