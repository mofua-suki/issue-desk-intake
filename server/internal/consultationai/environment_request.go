package consultationai

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"regexp"
	"strings"
)

// Worker 側の実装環境のネットワーク許可リストを出典とする。内容は未確認のため空から始める。
const environmentAllowedHosts = ""

type environmentFinding struct{ location, kind, line string }

var (
	envFence           = regexp.MustCompile("^\\s*(```+|~~~+)")
	envHeading         = regexp.MustCompile(`^\s*(#{1,6})\s+(.+?)\s*#*\s*$`)
	envGit             = regexp.MustCompile(`(?i)(?:^|[^\w./-])\.git(?:/|\b)|\bgit\s+(?:fetch|pull|merge|rebase|checkout|commit|push|switch|reset|clone)\b`)
	envBranch          = regexp.MustCompile(`(?i)(?:既定ブランチ|デフォルトブランチ|\bmain\b|\bmaster\b|\btrunk\b).{0,35}(?:取り込|追従|マージ|同期|pull|merge|rebase)|(?:取り込|追従|マージ|同期).{0,25}(?:既定ブランチ|デフォルトブランチ|\bmain\b|\bmaster\b|\btrunk\b)|最新.{0,15}(?:既定ブランチ|デフォルトブランチ|\bmain\b|\bmaster\b|\btrunk\b).{0,15}更新`)
	envBranchPath      = regexp.MustCompile(`(?i)\b(?:main|master|trunk)[./][a-z0-9_./-]+`)
	envConflict        = regexp.MustCompile(`(?i)(?:git|マージの|rebase|merge|ブランチ|\.git).{0,35}(?:競合|コンフリクト).{0,25}(?:確認|解消|解決|修正)|(?:競合|コンフリクト).{0,25}(?:git|マージの|rebase|merge|ブランチ).{0,25}(?:確認|解消|解決|修正)`)
	envNetwork         = regexp.MustCompile(`(?i:https?://[^\s\x60<>）)]+)|(?i:\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|org|net|io|ai|co|dev|jp|edu|gov|info|biz|uk|de|fr|app)\b)|(?i:\b(?:hugging\s*face|github|gitlab|openai|npmjs|pypi)\b)`)
	envFileExtension   = regexp.MustCompile(`(?i)\.(?:json|mjs|cjs|js|ts|tsx|jsx|go|yml|yaml|md|txt|sh|html|css|xml|toml|lock|sql|mod|sum|py|rs|java|csv|svg|png|test)$`)
	envNetworkAction   = regexp.MustCompile(`(?i)アクセス|呼び出|呼出|取得|ダウンロード|接続|問い合わせ|確認|検証|利用|使用|fetch|curl|wget|\bapi\b`)
	envNetworkExplicit = regexp.MustCompile(`(?i)アクセス|呼び出|呼出|取得|ダウンロード|接続|問い合わせ|fetch|curl|wget|\bapi\b`)
	envProduction      = regexp.MustCompile(`(?:本番(?:環境|の)?|実環境|実運用).{0,35}(?:DB|データベース|ログ|データ|測定|計測|取得)|(?:DB|データベース|ログ|データ).{0,20}(?:本番|実環境)`)
	envMeasure         = regexp.MustCompile(`(?i)測定|計測|取得|収集|調査|確認|参照|読む|分析|アクセス|接続|照合|採取`)
	envExcluded        = regexp.MustCompile(`(?i)(?:^|[、,])\s*(?:コントローラー|受付(?:AI)?|監視|worker|hostd|マージ後.{0,15}(?:人|利用者|担当者))\s*(?:が|は)\s*|(?:コントローラー|受付AI|監視|worker|hostd)(?:の担当|が行う)`)
	envNegative        = regexp.MustCompile(`(?i)(?:git\s+\w+|\.git(?:/\S*)?|取り込|追従|アクセス|接続|取得|測定|計測|呼び出|呼出|使用|利用|API)[^、,。；;]{0,30}?(?:しない|しません|せず|禁止|不要|求めない|使わない|行わない|触れない|できない|担当外|対象外|さない|ません)`)
	envPast            = regexp.MustCompile(`^(?:原因|経緯|過去|以前|前回)`)
)

func environmentAllowed(target string) bool {
	host := strings.ToLower(strings.TrimSpace(target))
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	for _, allowed := range strings.Fields(environmentAllowedHosts) {
		if host == allowed {
			return true
		}
	}
	return false
}

func environmentLineKind(line string) string {
	lower := strings.ToLower(line)
	if envExcluded.MatchString(lower) || envNegative.MatchString(lower) || envPast.MatchString(lower) {
		return ""
	}
	if envGit.MatchString(line) || envBranch.MatchString(envBranchPath.ReplaceAllString(line, "")) || envConflict.MatchString(line) {
		return "git・既定ブランチ"
	}
	if envProduction.MatchString(line) && envMeasure.MatchString(line) {
		return "本番データ"
	}
	if envNetworkAction.MatchString(line) {
		for _, match := range envNetwork.FindAllStringIndex(line, -1) {
			target := line[match[0]:match[1]]
			isURL := strings.HasPrefix(strings.ToLower(target), "http://") || strings.HasPrefix(strings.ToLower(target), "https://")
			if !isURL && match[1] < len(line) && line[match[1]] == '/' {
				continue
			}
			if !isURL && strings.Contains(target, ".") && !envNetworkExplicit.MatchString(line) {
				continue
			}
			if !isURL && (strings.EqualFold(target, "Dockerfile.dev") || envFileExtension.MatchString(target) || (match[0] > 0 && (line[match[0]-1] == '/' || line[match[0]-1] == '.' || line[match[0]-1] >= 'A' && line[match[0]-1] <= 'Z'))) {
				continue
			}
			if !environmentAllowed(target) {
				return "許可リスト外のネットワーク"
			}
		}
	}
	return ""
}

func environmentScan(location, value string) []environmentFinding {
	return environmentScanWith(location, value, environmentLineKind)
}

func environmentScanWith(location, value string, classify func(string) string) []environmentFinding {
	var findings []environmentFinding
	fence, excludedLevel := "", 0
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := envFence.FindStringSubmatch(line); m != nil {
			marker := m[1][:1]
			if fence == "" {
				fence = marker
			} else if fence == marker {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := envHeading.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			if excludedLevel > 0 && level <= excludedLevel {
				excludedLevel = 0
			}
			if strings.TrimSpace(m[2]) == "参照（経緯・根拠）" || strings.TrimSpace(m[2]) == "マージ後の運用作業（自動化の対象外）" {
				excludedLevel = level
			}
			continue
		}
		if excludedLevel > 0 || strings.HasPrefix(trimmed, ">") {
			continue
		}
		// 文ごとに判定し、同じ行の別の否定文・過去文による見逃しを防ぐ。
		for _, sentence := range strings.FieldsFunc(trimmed, func(r rune) bool { return r == '。' || r == '；' || r == ';' }) {
			if kind := classify(sentence); kind != "" {
				findings = append(findings, environmentFinding{location, kind, strings.TrimSpace(line)})
				break
			}
		}
	}
	return findings
}

func environmentRequests(answer Record) []environmentFinding {
	if answer["ready"] != true {
		return nil
	}
	found := environmentScan("body", fleet.Text(answer["body"]))
	for i, item := range fleet.Elements(answer["splitIssues"]) {
		child := fleet.Object(item)
		for _, key := range []string{"title", "purpose"} {
			found = append(found, environmentScan(fmt.Sprintf("splitIssues[%d].%s", i+1, key), fleet.Text(child[key]))...)
		}
		for j, item := range fleet.Elements(child["acceptance"]) {
			found = append(found, environmentScan(fmt.Sprintf("splitIssues[%d].acceptance[%d]", i+1, j+1), fleet.Text(item))...)
		}
	}
	return found
}

func environmentRequestDetails(findings []environmentFinding) string {
	var lines []string
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("%s（%s）: %q", f.location, f.kind, f.line))
	}
	return strings.Join(lines, "\n")
}
