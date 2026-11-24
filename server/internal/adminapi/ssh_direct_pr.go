package adminapi

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

type directPRProof struct {
	repo, branch, head, base string
}

var directPRBranch = regexp.MustCompile(`^chat/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)*$`)

// parseDirectPRProof checks raw Git commit objects from head to base without
// assuming a caller-supplied SHA represents the object it claims to verify.
func parseDirectPRProof(raw any) (directPRProof, bool) {
	input := obj(raw)
	p := directPRProof{repo: str(input["repo"]), branch: str(input["branch"]), head: str(input["head"]), base: str(input["base"])}
	commits, ok := input["commits"].([]any)
	if !ok || len(input) != 5 || len(commits) < 1 || len(commits) > 16 ||
		!directPRBranch.MatchString(p.branch) || len(p.branch) > 160 ||
		!directPRHead.MatchString(p.head) || !directPRHead.MatchString(p.base) || p.head == p.base {
		return directPRProof{}, false
	}
	expected := p.head
	for _, value := range commits {
		if len(str(value)) > 16384 {
			return directPRProof{}, false
		}
		rawCommit, err := base64.StdEncoding.Strict().DecodeString(str(value))
		if err != nil {
			return directPRProof{}, false
		}
		hash := sha1.New()
		fmt.Fprintf(hash, "commit %d\x00", len(rawCommit))
		_, _ = hash.Write(rawCommit)
		lines := strings.SplitN(string(rawCommit), "\n", 3)
		if fmt.Sprintf("%x", hash.Sum(nil)) != expected || len(lines) != 3 ||
			!strings.HasPrefix(lines[0], "tree ") || !directPRHead.MatchString(strings.TrimPrefix(lines[0], "tree ")) ||
			!strings.HasPrefix(lines[1], "parent ") {
			return directPRProof{}, false
		}
		expected = strings.TrimPrefix(lines[1], "parent ")
		if !directPRHead.MatchString(expected) {
			return directPRProof{}, false
		}
	}
	return p, expected == p.base
}
