package releasepolicy

import "testing"

func TestSSHDevelopmentRequiresPermissionApproval(t *testing.T) {
	files := Files("mofua-suki/issue-desk-intake")
	for _, name := range []string{"scripts/ssh-development-host.sh", "scripts/ssh-development-runner.ts", "scripts/ssh-verify-host.sh", "scripts/ssh-verify-runner.ts"} {
		found := false
		for _, file := range files {
			if file == name {
				found = true
			}
		}
		if !found {
			t.Errorf("SSHの実行境界を変えるファイルは承認が必要です: %s", name)
		}
	}
}
