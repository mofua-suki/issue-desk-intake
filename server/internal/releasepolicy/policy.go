// Package releasepolicy supplies the controller-owned file sets used when
// approving changes to permission boundaries. Fleet input cannot replace them.
package releasepolicy

import "strings"

var common = []string{
	"host-ssh.json", "scripts/ssh-development-host.sh", "scripts/ssh-development-runner.ts", "scripts/ssh-verify-host.sh", "scripts/ssh-verify-runner.ts", "server/src/ssh-tickets.ts", "server/src/ssh-ticket-routes.ts",
	"deploy/nas-host/ssh-sign.sh", "deploy/nas-host/setup-admin.sh",
	"deploy/client/ssh/setup-ssh.ps1", "deploy/client/hostd/ssh-repair.ps1",
	"deploy/nas/remote-control-settings.json", "deploy/nas/claude-rc@.service",
	"deploy/nas/permission-grant-rules.json", "deploy/nas/permission-grants-apply.ts",
	"shared/src/permission-guard.ts",
}
var worker = []string{
	"deploy/client/ssh/setup-ssh.ps1", "deploy/client/hostd/ssh-repair.ps1",
	"internal/execution/guards.go",
}
var intake = []string{
	"host-ssh.json", "scripts/ssh-development-host.sh", "scripts/ssh-development-runner.ts", "scripts/ssh-verify-host.sh", "scripts/ssh-verify-runner.ts",
	"server/internal/adminapi/ssh.go", "server/internal/adminapi/host_ssh_policy.go",
	"server/internal/adminapi/api.go", "server/internal/adminapi/sql.go",
	"deploy/nas-host/ssh-sign.sh", "deploy/nas-host/setup-admin.sh",
	"deploy/nas/remote-control-settings.json", "deploy/nas/claude-rc@.service",
	"deploy/nas/permission-grant-rules.json", "deploy/nas/permission-grants-apply.ts",
	"server/internal/releasepolicy/policy.go",
}

func Files(repository string) []string {
	list := common
	switch strings.ToLower(repository) {
	case "mofua-suki/issue-desk-worker":
		list = worker
	case "mofua-suki/issue-desk-intake":
		list = intake
	}
	return append([]string{}, list...)
}

// Configuration contains only authoritative sets for the repositories assigned
// to a host. Caller-provided settings never select or alter these sets.
func Configuration(repositories []string) map[string]any {
	byRepo := map[string]any{}
	for _, repo := range repositories {
		byRepo[repo] = map[string]any{"permissionFiles": Files(repo)}
	}
	return map[string]any{"repositories": byRepo}
}
