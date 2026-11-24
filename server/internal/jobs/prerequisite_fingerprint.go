package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PrerequisiteFingerprint identifies a reported cause without persisting its text.
// Artifact order and repository case do not change the identity.
func PrerequisiteFingerprint(missing any) (string, error) {
	raw, err := json.Marshal(missing)
	if err != nil {
		return "", err
	}
	var m struct {
		Reason    string `json:"reason"`
		Artifacts []struct {
			Artifact string `json:"artifact"`
			Repo     string `json:"repo"`
			Issue    int64  `json:"issue"`
		} `json:"artifacts"`
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	m.Reason = strings.TrimSpace(m.Reason)
	if m.Reason == "" || len(m.Artifacts) == 0 {
		return "", fmt.Errorf("missing prerequisite cause")
	}
	artifacts := make([]string, 0, len(m.Artifacts))
	for _, a := range m.Artifacts {
		if strings.TrimSpace(a.Artifact) == "" || !repositoryName.MatchString(a.Repo) || a.Issue < 1 {
			return "", fmt.Errorf("invalid prerequisite artifact")
		}
		b, _ := json.Marshal([]any{strings.TrimSpace(a.Artifact), strings.ToLower(a.Repo), a.Issue})
		artifacts = append(artifacts, string(b))
	}
	sort.Strings(artifacts)
	canonical, _ := json.Marshal([]any{m.Reason, artifacts})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
