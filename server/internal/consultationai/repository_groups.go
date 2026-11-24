package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

// configuredRepositoryGroup resolves only names already present in the read allowlist.
func configuredRepositoryGroup(config map[string]any, selected string, allowed map[string]bool) []string {
	for _, raw := range fleet.Elements(fleet.Object(config["assignments"])["repositoryGroups"]) {
		members := fleet.Elements(raw)
		found := false
		for _, member := range members {
			if strings.EqualFold(fleet.Text(member), selected) {
				found = true
			}
		}
		if !found {
			continue
		}
		result := make([]string, 0, len(members))
		for _, member := range members {
			name := fleet.Text(member)
			for candidate := range allowed {
				if strings.EqualFold(candidate, name) {
					name = candidate
					break
				}
			}
			// Keep invalid members visible to the existing permission check; never grant access.
			result = append(result, name)
		}
		return result
	}
	return nil
}

func explicitGroupConsent(message string) bool {
	switch strings.TrimSpace(message) {
	case "了承します", "承認します":
		return true
	default:
		return false
	}
}

func (m *materials) bootstrapGroup(ctx context.Context) error {
	project := fleet.Text(m.draft["project"])
	for _, member := range m.group {
		if strings.EqualFold(member, project) {
			continue
		}
		if m.state.Projects[member] != nil {
			continue // Turn has already restored every saved project above.
		}
		if err := m.bootstrap(ctx, member); err != nil {
			return err
		}
	}
	return nil
}

// checkRepositoryGroup holds a ready answer until each member has a delivered,
// reviewed body and a proposal, or a specifically described approved exception.
func (m *materials) checkRepositoryGroup(answer Record, cross map[string]any, scopes []map[string]string) error {
	if len(m.group) == 0 {
		return nil
	}
	parent := fleet.Text(m.draft["project"])
	wasReady := answer["ready"] == true
	plans := plannedProjects(answer, parent)
	reviewed := reviewedProjectFiles(answer, parent)
	for _, member := range m.group {
		if !m.allowed[member] {
			return errors.New("組に許可対象外のプロジェクトがあります")
		}
		if !m.memberBodyReviewed(member, reviewed) && wasReady {
			controllerQuestion(answer, member+"の固定版資料の本文を確認してください。", "組の全対象の本文確認が必要です")
		}
		if plans[member] {
			for _, scope := range scopes {
				if scope["project"] == member && (strings.HasPrefix(scope["scope"], "除外:") || strings.HasPrefix(scope["scope"], "除外：")) && wasReady {
					controllerQuestion(answer, member+"の案と除外指定の食い違いを修正してください。", "案の対象を除外として了承できません")
				}
			}
			continue
		}
		if !describedGroupException(member, scopes, cross) && wasReady {
			controllerQuestion(answer, member+"の案を追加するか、除外の対象・条件と理由を示してください。", "組の各対象に案または明示了承する除外が必要です")
		}
	}
	return bindGroupReason(scopes, cross)
}

// plannedProjects lists the projects that have an issue in the answer.
func plannedProjects(answer Record, parent string) map[string]bool {
	plans := map[string]bool{}
	for _, item := range fleet.Elements(answer["splitIssues"]) {
		name := fleet.Text(fleet.Object(item)["project"])
		if name == "" {
			name = parent
		}
		plans[name] = true
	}
	if len(fleet.Elements(answer["splitIssues"])) == 0 {
		plans[parent] = true
	}
	return plans
}

// reviewedProjectFiles lists the reviewed files as project:path.
func reviewedProjectFiles(answer Record, parent string) map[string]bool {
	reviewed := map[string]bool{}
	for _, item := range fleet.Elements(answer["reviewedFiles"]) {
		file := reviewedFilePath(fleet.Text(item))
		if !strings.Contains(file, ":") {
			file = parent + ":" + file
		}
		reviewed[file] = true
	}
	return reviewed
}

// memberBodyReviewed reports whether a body document of the member's pinned revision was
// delivered and reviewed.
func (m *materials) memberBodyReviewed(member string, reviewed map[string]bool) bool {
	body := false
	if p := m.state.Projects[member]; p != nil && sha40.MatchString(p.Revision) {
		documents := append(append([]Document{}, p.Reads...), m.evidence.documents...)
		for _, doc := range documents {
			if doc.Project == member && bodyDocument(doc) && doc.Version == p.Revision && doc.SHA256 != "" && doc.Size > 0 && m.state.Delivered[documentKey(doc)] && reviewed[member+":"+doc.Path] {
				body = true
			}
		}
	}
	return body
}

// describedGroupException reports whether the member is excluded with a condition and a reason.
func describedGroupException(member string, scopes []map[string]string, cross map[string]any) bool {
	exception := false
	for _, scope := range scopes {
		if scope["project"] != member {
			continue
		}
		description := strings.TrimSpace(scope["scope"])
		reason := strings.Join(strings.Fields(fleet.Text(cross["reason"])), " ")
		if reason != "" {
			description = strings.TrimSuffix(description, "（理由: "+reason+"）")
		}
		condition := ""
		for _, prefix := range []string{"除外:", "除外："} {
			if strings.HasPrefix(description, prefix) {
				condition = strings.TrimSpace(strings.TrimPrefix(description, prefix))
				break
			}
		}
		if condition != "" && reason != "" {
			exception = true
		}
	}
	return exception
}

// bindGroupReason binds the reason to the first sorted scope for grouped consultations while
// retaining the existing scope-only fingerprint format used by filing.
func bindGroupReason(scopes []map[string]string, cross map[string]any) error {
	if len(scopes) > 0 && strings.TrimSpace(fleet.Text(cross["reason"])) != "" {
		first := 0
		for i := 1; i < len(scopes); i++ {
			if scopes[i]["project"] < scopes[first]["project"] {
				first = i
			}
		}
		suffix := "（理由: " + strings.Join(strings.Fields(fleet.Text(cross["reason"])), " ") + "）"
		if !strings.HasSuffix(scopes[first]["scope"], suffix) {
			scopes[first]["scope"] += suffix
		}
		if contract.UTF16Length(scopes[first]["scope"]) > 2000 {
			return errors.New("対象範囲と理由が長すぎます")
		}
	}
	return nil
}
