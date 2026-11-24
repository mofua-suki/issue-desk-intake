package filing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strconv"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
)

var settingsTrailer = regexp.MustCompile(`^\r?\n<!-- issue-desk:settings (\{[^<>]*\}) -->`)
var estimateSection = regexp.MustCompile(`(?m)^## 見込みの大きさ[ \t]*\r?\n変更行 約([0-9]+)行・変更ファイル ([0-9]+)本[ \t]*(?:\r?\n|$)`)

func transferInput(issue map[string]any) Record {
	body := fleet.Text(issue["body"])
	sum := sha256.Sum256([]byte(body))
	return Record{"title": fleet.Text(issue["title"]), "body": contract.ClipUTF16(body, 12000), "complete": contract.UTF16Length(body) <= 12000, "revision": hex.EncodeToString(sum[:])}
}
func JobFromIssue(repo string, issue map[string]any, kind string) (jobs.NewJob, error) {
	number, valid := integer(issue["number"])
	if !valid || number < 1 {
		return jobs.NewJob{}, fail(503, "起票済みIssue番号を確認できません。")
	}
	body := fleet.Text(issue["body"])
	settings := issueSettings(body)
	if kind == "" {
		kind = issueKind(body)
	}
	if kind == "investigation" {
		return jobs.NewJob{}, contract.NewProblem(409, "調査Issueの実行はできません。", "investigation_disabled")
	}
	priority := 0
	if settings["priority"] == "high" {
		priority = 1
	}
	view := Record{"workType": normalizedWork(settings["workType"]), "acceptanceTransferInput": transferInput(issue)}
	if estimate, ok := issueEstimate(body); ok {
		view["estimate"] = estimate
	}
	var provider, tier *string
	if chosen := fleet.Text(settings["provider"]); chosen == "codex" || chosen == "claude" || chosen == "antigravity" {
		provider = &chosen
	}
	if level := fleet.Text(settings["tier"]); validTier(level) {
		tier = &level
	}
	parsed := ParseScope(body)
	scope := []string(nil)
	source := "estimated"
	if parsed.State == "declared" {
		scope = parsed.Paths
		source = "declared"
		if parsed.Backfilled {
			source = "estimated"
		}
	}
	targets, err := issueDependencies(repo, body)
	if err != nil {
		return jobs.NewJob{}, err
	}
	if len(targets) > 0 {
		view["dependsOnIssues"] = targets
	}
	job := jobs.NewJob{Repo: repo, Issue: number, Title: fleet.Text(issue["title"]), Kind: kind, Priority: priority, Scope: scope, ScopeSource: source, Provider: provider, Tier: tier, View: view}
	if at, err := time.Parse(time.RFC3339Nano, fleet.Text(issue["created_at"])); err == nil {
		job.FiledAt = &at
	}
	return job, nil
}

// issueSettings reads the settings trailer after the last filing marker.
func issueSettings(body string) Record {
	settings := Record{}
	locations := marker.FindAllStringSubmatchIndex(body, -1)
	if len(locations) > 0 {
		last := locations[len(locations)-1]
		if found := settingsTrailer.FindStringSubmatch(body[last[1]:]); len(found) > 1 {
			decoder := json.NewDecoder(strings.NewReader(found[1]))
			decoder.UseNumber()
			_ = decoder.Decode(&settings)
		}
	}
	return settings
}

// issueKind derives the job kind from the markers in the body.
func issueKind(body string) string {
	kind := "normal"
	if strings.Contains(body, "<!-- issue-desk-stall:") {
		kind = "stall_fix"
	} else if strings.Contains(body, "<!-- issue-desk-followup:") || strings.Contains(body, "<!-- issue-desk:followup -->") {
		kind = "followup"
	}
	return kind
}

// issueEstimate reads the expected change size section.
func issueEstimate(body string) (Record, bool) {
	if found := estimateSection.FindStringSubmatch(body); len(found) > 2 {
		lines, e1 := strconv.ParseInt(found[1], 10, 64)
		files, e2 := strconv.ParseInt(found[2], 10, 64)
		if e1 == nil && e2 == nil && lines >= 0 && files >= 0 && lines <= 9007199254740991 && files <= 9007199254740991 {
			return Record{"lines": lines, "files": files}, true
		}
	}
	return nil, false
}

// issueDependencies reads the dependency declaration of a filed issue.
func issueDependencies(repo, body string) ([]any, error) {
	at := strings.LastIndex(body, "<!-- issue-desk-dependencies:")
	if at < 0 {
		return nil, nil
	}
	tail := body[at+len("<!-- issue-desk-dependencies:"):]
	end := strings.Index(tail, " -->")
	if end < 0 {
		return nil, fail(409, "起票済みIssueの依存宣言が閉じていません。")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(tail[:end]))
	decoder.UseNumber()
	var refs []any
	if decoder.Decode(&refs) != nil {
		return nil, fail(409, "起票済みIssueの依存宣言を読み取れません。")
	}
	targets := []any{}
	for _, raw := range refs {
		targetRepo := repo
		value, ok := integer(raw)
		if !ok {
			reference := fleet.Text(raw)
			split := strings.LastIndex(reference, "#")
			if split < 1 {
				return nil, fail(409, "起票済みIssueの先行指定が不正です。")
			}
			targetRepo = reference[:split]
			value, _ = strconv.ParseInt(reference[split+1:], 10, 64)
		}
		if !repositoryPattern.MatchString(targetRepo) || value < 1 || value > 9007199254740991 {
			return nil, fail(409, "起票済みIssueの先行指定が不正です。")
		}
		targets = append(targets, Record{"repo": targetRepo, "issue": value})
	}
	return targets, nil
}
