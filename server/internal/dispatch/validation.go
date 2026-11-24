package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

var RequestID = regexp.MustCompile(`^\d{1,16}-[a-f0-9]{8}$`)
var commandID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)
var sha = regexp.MustCompile(`^[a-f0-9]{40}$`)

// now is the clock for the instruction time of an approval (replaced in tests).
var now = time.Now

func positive(value any) bool {
	v, ok := value.(float64)
	return ok && v >= 1 && v <= 9007199254740991 && v == math.Trunc(v)
}

func ValidatePayload(kind, repo string, payload map[string]any) (map[string]any, error) {
	if !fleet.ValidRepo(repo) {
		return nil, problem(400, "リポジトリ名が不正です。")
	}
	out := map[string]any{}
	switch kind {
	case "native_test":
		if len(payload) != 4 || !fleet.ValidHost(fleet.Text(payload["host"])) {
			return nil, problem(400, "Mac検証のホストが不正です。")
		}
		out["host"] = payload["host"]
		for _, key := range []string{"head", "base", "tree"} {
			if !sha.MatchString(fleet.Text(payload[key])) {
				return nil, problem(400, "Mac検証にはhead・base・treeのSHAが必要です。")
			}
			out[key] = payload[key]
		}
	case "pr_close":
		if !positive(payload["number"]) || !positive(payload["issue"]) {
			return nil, problem(400, "PRとIssueは正の整数です。")
		}
		out["number"], out["issue"] = payload["number"], payload["issue"]
	case "full_test":
		if len(payload) != 0 {
			return nil, problem(400, "main確認の対象が不正です。")
		}
	case "merge_manual":
		if !positive(payload["number"]) || !sha.MatchString(fleet.Text(payload["head"])) {
			return nil, problem(400, "PR番号と先頭SHAが必要です。")
		}
		out["number"], out["head"] = payload["number"], payload["head"]
		// 利用者の承認発言。IssueのPR(codex/issue-*)・権限保護対象・データ削除SQLをマージするときだけ、Workerが要求する。
		approved, err := approvalFields(payload, out)
		if err != nil {
			return nil, err
		}
		// approvalRequest は、PR番号が決まる前に作成依頼（pr_create）へ付けた事前承認を指す。受付が照合して承認を写す（Submit）。
		if value, exists := payload["approvalRequest"]; exists {
			if approved {
				return nil, problem(400, "approvalRequestは承認発言（approval）と同時に指定できません。")
			}
			if !RequestID.MatchString(fleet.Text(value)) {
				return nil, problem(400, "approvalRequestは依頼IDの形（<数字>-<16進8桁>）です。")
			}
			out["approvalRequest"] = value
		}
	case "pr_create":
		if payload["emergency"] == true {
			return nil, problem(400, "緊急PRは受け付けません。")
		}
		if !contract.FilledText(payload["title"], 255) || strings.ContainsAny(fleet.Text(payload["title"]), "\r\n") || !contract.FilledText(payload["body"], 60000) {
			return nil, problem(400, "PRの題と本文が不正です。")
		}
		out["title"], out["body"] = payload["title"], payload["body"]
		// 事前承認。PR番号が決まる前の利用者の承認を作成依頼に残し、マージ依頼の approvalRequest から使う。
		if _, err := approvalFields(payload, out); err != nil {
			return nil, err
		}
		_, patch := payload["patch"]
		_, base := payload["base"]
		_, hash := payload["patchSha256"]
		if patch || base || hash {
			value := fleet.Text(payload["patch"])
			sum := sha256.Sum256([]byte(value))
			if _, branch := payload["branch"]; branch || strings.TrimSpace(value) == "" || len([]byte(value)) > 200000 || strings.ContainsRune(value, 0) || strings.Contains(value, "GIT binary patch") || !sha.MatchString(fleet.Text(payload["base"])) || hex.EncodeToString(sum[:]) != payload["patchSha256"] {
				return nil, problem(400, "パッチ・基点・SHA256が不正です。")
			}
			for _, key := range []string{"patch", "base", "patchSha256"} {
				out[key] = payload[key]
			}
		} else {
			branch := fleet.Text(payload["branch"])
			if branch == "" || strings.HasPrefix(branch, "-") || !regexp.MustCompile(`^[A-Za-z0-9._/-]+$`).MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") || strings.HasPrefix(branch, "codex/issue-") {
				return nil, problem(400, "直接PRのブランチ名が不正です。")
			}
			out["branch"] = branch
		}
	default:
		return nil, problem(400, "依頼の種類が不正です。")
	}
	return out, nil
}

// approvalFields は利用者の承認発言（500文字以内）と、利用者がチャットで指示した日時（instructionAt、必須）を検査して out に写す。
// approvalDelegated は受付が認証から付けた印で、承認発言があるときだけ残す。承認発言があれば true を返す。
func approvalFields(payload, out map[string]any) (bool, error) {
	approved := false
	if value, exists := payload["approval"]; exists {
		approval, ok := value.(string)
		if !ok || len([]rune(strings.TrimSpace(approval))) > 500 {
			return false, problem(400, "承認発言は500文字以内の文字列です。")
		}
		if strings.TrimSpace(approval) != "" {
			approved = true
			out["approval"] = strings.TrimSpace(approval)
			if payload["approvalDelegated"] == true {
				out["approvalDelegated"] = true
			}
			at, ok := contract.InstructionAt(payload["instructionAt"], now())
			if !ok {
				return false, problem(400, contract.InstructionAtProblem)
			}
			out["instructionAt"] = at
		}
	}
	if _, exists := payload["instructionAt"]; exists && !approved {
		return false, problem(400, "instructionAtは承認発言（approval）と一緒に指定してください。")
	}
	return approved, nil
}

func ValidateResult(kind string, result map[string]any) error {
	if kind == "full_test" && (result["status"] == "passed" || result["status"] == "failed") && !validFullTest(result) {
		return problem(400, "mainのテスト結果の形が違います。")
	}
	if kind == "native_test" {
		for _, key := range []string{"head", "base", "tree"} {
			if !sha.MatchString(fleet.Text(result[key])) {
				return problem(400, "Mac検証結果のSHAが不正です。")
			}
		}
		message, ok := result["message"].(string)
		if !ok || contract.UTF16Length(message) > 3000 {
			return problem(400, "Mac検証結果のmessageは3000文字以内です。")
		}
	}
	allowed := map[string][]string{"native_test": {"passed", "failed", "unrunnable", "interrupted"}, "pr_close": {"closed", "skipped", "refused", "failed"}, "full_test": {"passed", "failed", "unrunnable", "interrupted"}, "pr_create": {"created", "refused", "failed"}, "merge_manual": {"merged", "refused", "failed"}}
	for _, status := range allowed[kind] {
		if result["status"] == status {
			return nil
		}
	}
	return problem(400, "依頼結果のstatusが不正です。")
}

func validFullTest(result map[string]any) bool {
	started, a := time.Parse(time.RFC3339Nano, fleet.Text(result["startedAt"]))
	finished, b := time.Parse(time.RFC3339Nano, fleet.Text(result["finishedAt"]))
	files, ok := result["files"].(float64)
	elapsed, timeOK := result["ms"].(float64)
	failed, failedOK := result["failedFiles"].([]any)
	failures := fleet.Object(result["failures"])
	names, namesOK := failures["names"].([]any)
	_, completeOK := failures["complete"].(bool)
	if !sha.MatchString(fleet.Text(result["sha"])) || a != nil || b != nil || finished.Before(started) || !ok || files < 0 || files != math.Trunc(files) || files > 9007199254740991 || !timeOK || math.IsNaN(elapsed) || math.IsInf(elapsed, 0) || elapsed < 0 || !failedOK || len(failed) > 200 || !namesOK || len(names) > 200 || !completeOK {
		return false
	}
	for _, value := range failed {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	for _, value := range names {
		v, ok := value.(string)
		if !ok || contract.UTF16Length(v) > 1000 {
			return false
		}
	}
	excerpt, ok := result["excerpt"].(string)
	if !ok || contract.UTF16Length(excerpt) > 3000 {
		return false
	}
	return result["status"] != "passed" || len(failed) == 0 && len(names) == 0 && failures["complete"] == true
}
