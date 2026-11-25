package filing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
)

var handoffInput = regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:[@:]([a-f0-9]{8,40}))?:([A-Za-z0-9_./-]+):([0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)*)`)
var handoffRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var handoffDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Check the saved handoff again at the filing boundary, including after edits.
func validateHandoff(c BuildConfig, d Record, plan *workplan.Plan) error {
	var texts []string
	texts = append(texts, fleet.Text(d["body"]))
	if plan != nil {
		for _, r := range plan.Requirements {
			if r.EvidenceKind != "external_check" {
				texts = append(texts, r.Inputs...)
			}
		}
	}
	docs := fleet.Elements(row(d["context"])["handoff"])
	checked := map[string]bool{}
	for _, raw := range docs {
		item := row(raw)
		if !validHandoffDocument(item) {
			return fail(409, "保存された資料本文を確認できません")
		}
		repo := fleet.Text(item["project"])
		if checked[repo] {
			continue
		}
		if c.Project == nil {
			return fail(503, "資料の読取許可を確認できません: "+repo)
		}
		if err := c.Project(repo); err != nil {
			return fail(409, "資料の読取が許可されていません: "+repo)
		}
		checked[repo] = true
	}
	for _, text := range texts {
		for _, match := range handoffInput.FindAllStringSubmatch(text, -1) {
			if match[1] == fleet.Text(d["project"]) {
				continue
			}
			for _, span := range strings.Split(match[4], ",") {
				a, b, ok := strings.Cut(span, "-")
				if !ok {
					b = a
				}
				start, e1 := strconv.Atoi(a)
				end, e2 := strconv.Atoi(b)
				if e1 != nil || e2 != nil || start < 1 || end < start {
					return fail(409, "資料の行範囲が不正です: "+match[0])
				}
				found := false
				for _, raw := range docs {
					v := row(raw)
					revision := fleet.Text(v["revision"])
					if fleet.Text(v["project"]) != match[1] || fleet.Text(v["path"]) != match[3] || !handoffRevision.MatchString(revision) || !handoffDigest.MatchString(fleet.Text(v["sha256"])) || match[2] != "" && !strings.HasPrefix(revision, match[2]) {
						continue
					}
					from, to := int(fleet.Number(v["start"])), int(fleet.Number(v["end"]))
					if from != start || to != end {
						continue
					}
					lines := strings.Split(fleet.Text(v["text"]), "\n")
					sum := sha256.Sum256([]byte(fleet.Text(v["text"])))
					if hex.EncodeToString(sum[:]) != fleet.Text(v["textSha256"]) {
						continue
					}
					if len(lines) != end-start+1 {
						continue
					}
					valid := true
					for i, line := range lines {
						if !strings.HasPrefix(line, fmt.Sprintf("%d: ", start+i)) {
							valid = false
							break
						}
					}
					if valid {
						found = true
						break
					}
				}
				if !found {
					return fail(409, "必要資料の固定版本文がありません: "+match[0])
				}
			}
		}
	}
	return nil
}

func validHandoffDocument(v Record) bool {
	if v == nil || !handoffRevision.MatchString(fleet.Text(v["revision"])) || !handoffDigest.MatchString(fleet.Text(v["sha256"])) {
		return false
	}
	start, end := int(fleet.Number(v["start"])), int(fleet.Number(v["end"]))
	if start < 1 || end < start {
		return false
	}
	text := fleet.Text(v["text"])
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != fleet.Text(v["textSha256"]) {
		return false
	}
	lines := strings.Split(text, "\n")
	if len(lines) != end-start+1 {
		return false
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, fmt.Sprintf("%d: ", start+i)) {
			return false
		}
	}
	return true
}
