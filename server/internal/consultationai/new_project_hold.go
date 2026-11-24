package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// holdNewProject also handles answers saved before new handoffs were disabled.
func holdNewProject(answer Record) {
	if fleet.Object(answer["newProject"])["needed"] != true {
		return
	}
	answer["ready"] = false
	const notice = "新しいリポジトリが必要な場合は、チャットのAIに相談してください。"
	if !strings.Contains(fleet.Text(answer["reply"]), notice) {
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n" + notice)
	}
}
