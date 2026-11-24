package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func quoted(value string) string { data, _ := json.Marshal(value); return string(data) }
func toml(value any) string {
	if object, ok := value.(map[string]any); ok {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, quoted(key)+"="+toml(object[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	data, _ := json.Marshal(value)
	return string(data)
}

type Security struct {
	RuntimeDir      string
	ApplicationDir  string
	TokenFiles      []string
	ToolExecutables []string
}

func policy(config Security, workspace string, dataOnly bool) []string {
	filesystem := map[string]any{":root": "read", ":minimal": "read"}
	for _, name := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if value := os.Getenv(name); value != "" {
			filesystem[filepath.Clean(value)] = "deny"
		}
	}
	for _, exe := range config.ToolExecutables {
		if filepath.IsAbs(exe) {
			filesystem[filepath.Dir(exe)] = "read"
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				filesystem[filepath.Dir(real)] = "read"
			}
		}
	}
	for _, file := range config.TokenFiles {
		if file != "" {
			filesystem[filepath.Dir(file)] = "deny"
		}
	}
	if config.RuntimeDir != "" {
		filesystem[filepath.Join(config.RuntimeDir, "state")] = "deny"
		filesystem[filepath.Join(config.RuntimeDir, "control")] = "deny"
	}
	if config.ApplicationDir != "" {
		filesystem[config.ApplicationDir] = "deny"
	}
	filesystem[workspace] = "read"
	// This governs model tools, not the CLI's own subscription transport.
	profile := map[string]any{"extends": ":read-only", "filesystem": filesystem, "network": map[string]any{"enabled": !dataOnly, "domains": map[string]any{}}}
	values := map[string]any{"approval_policy": "never", "default_permissions": "gitea-worker", "web_search": "disabled", "features.network_proxy": false, "features.multi_agent": false, "features.apps": false, "features.plugins": false, "features.hooks": false, "features.computer_use": false, "features.browser_use": false, "features.in_app_browser": false, "features.workspace_dependencies": false, "features.skill_search": false, "features.skip_host_skill_discovery": true, "features.unbounded_connection_retries": false, "features.shell_tool": false, "features.unified_exec": false, "features.code_mode_host": false, "features.image_generation": false, "features.view_image": false}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys)+2)
	for _, key := range keys {
		args = append(args, "-c", key+"="+toml(values[key]))
	}
	return append(args, "-c", "permissions="+toml(map[string]any{"gitea-worker": profile}))
}

func ClaudeSettings(config Security, workspace string) map[string]any {
	deny := []string{}
	for _, name := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if value := os.Getenv(name); value != "" {
			deny = append(deny, "Read("+filepath.Clean(value)+"/**)", "Edit("+filepath.Clean(value)+"/**)", "Write("+filepath.Clean(value)+"/**)")
		}
	}
	for _, file := range config.TokenFiles {
		if file != "" {
			deny = append(deny, "Read("+filepath.Dir(file)+"/**)")
		}
	}
	for _, dir := range []string{filepath.Join(config.RuntimeDir, "control"), filepath.Join(config.RuntimeDir, "state"), config.ApplicationDir} {
		if dir != "" {
			deny = append(deny, "Read("+dir+"/**)")
		}
	}
	return map[string]any{"permissions": map[string]any{"deny": deny, "allow": []string{"Read(" + workspace + "/**)", "Glob(" + workspace + "/**)", "Grep(" + workspace + "/**)"}}, "env": map[string]any{}}
}
