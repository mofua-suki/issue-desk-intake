package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"
)

var (
	capabilityRace    = regexp.MustCompile(`(?i)(?:\brace\b|-race|レース検(?:査|証|出))`)
	capabilityGCC     = regexp.MustCompile(`(?i)\bgcc\b`)
	capabilityCGO     = regexp.MustCompile(`(?i)\bcgo\b`)
	capabilityDocker  = regexp.MustCompile(`(?i)\bdocker\b`)
	capabilityWindows = regexp.MustCompile(`(?i)\bwindows\b`)
	capabilityMac     = regexp.MustCompile(`(?i)\b(?:mac\s*os[0-9]*|darwin)\b`)
	// Paths (including Go packages and subtests) name what to run, not the host OS.
	// A standalone OS/architecture pair or OS pair still names an environment.
	capabilityPlatformPath = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+[/\\])+[a-z0-9_.-]+|[a-z0-9_.-]+\.[a-z][a-z0-9]*\b`)
	capabilityPlatformPair = regexp.MustCompile(`(?i)^(?:windows|linux|darwin|macos[0-9.]*)/(?:windows|linux|darwin|macos[0-9.]*|amd64|x86[_-]64|x64|x86|386|i[3-6]86|arm64|aarch64|arm(?:v[5-9]l?)?)\.?$`)
	capabilityDevice       = regexp.MustCompile(`実機|実端末|実デバイス`)
	capabilityLive         = regexp.MustCompile(`(?:実環境|本番環境|実運用|本番).{0,30}(?:実測|測定|計測)|(?:実測|測定|計測).{0,30}(?:実環境|本番環境|実運用|本番)`)
	capabilityAction       = regexp.MustCompile(`(?i)テスト|検査|検証|実行|確認|測定|計測|実測|必要|必須|要求|\btest\b|\bbuild\b|-race`)
	capabilityNegation     = regexp.MustCompile(`(?i)(?:race|gcc|cgo|docker|windows|macos|darwin|実機|実端末|実デバイス|実測|測定|計測)[^、,。；;]{0,35}(?:不要|しない|しません|せず|必要ない|必要ありません|求めない|対象外|行わない|使わない|禁止)`)
	capabilityMention      = regexp.MustCompile(`(?i)\brace\b|\bgcc\b|\bcgo\b|\bdocker\b|\bwindows\b|\bmacos\b|\bdarwin\b|実機|実端末|実デバイス|実測|測定|計測`)
	capabilityJoin         = regexp.MustCompile(`^\s*(?:と|や|及び|および|・|/|、|,)\s*$`)
)

// Extend a negation over a list of capabilities, but not an independent clause.
func removeNegatedCapabilities(sentence string) string {
	mentions := capabilityMention.FindAllStringIndex(sentence, -1)
	for i := len(mentions) - 1; i >= 0; i-- {
		end := len(sentence)
		if i+1 < len(mentions) {
			end = mentions[i+1][0]
		}
		start := mentions[i][0]
		if match := capabilityNegation.FindStringIndex(sentence[start:end]); match != nil && match[0] == 0 {
			negatedEnd := start + match[1]
			for i > 0 && capabilityJoin.MatchString(sentence[mentions[i-1][1]:start]) {
				i--
				start = mentions[i][0]
			}
			sentence = sentence[:start] + sentence[negatedEnd:]
		}
	}
	return sentence
}

// requiredCapabilities lists the host capabilities that the requirement sentence names.
func requiredCapabilities(sentence string) map[string]any {
	sentence = strings.TrimSpace(strings.TrimLeft(sentence, "- *0123456789.)"))
	if envExcluded.MatchString(sentence) || envPast.MatchString(sentence) {
		return nil
	}
	// Remove only the negated requirement; another clause in the same
	// sentence may independently require a different capability.
	sentence = removeNegatedCapabilities(sentence)
	sentence = envNegative.ReplaceAllString(sentence, "")
	if !capabilityAction.MatchString(sentence) {
		return nil
	}
	required := map[string]any{}
	if capabilityRace.MatchString(sentence) {
		required["race"], required["gcc"], required["cgo"] = true, true, true
	}
	if capabilityGCC.MatchString(sentence) {
		required["gcc"] = true
	}
	if capabilityCGO.MatchString(sentence) {
		required["cgo"] = true
	}
	if capabilityDocker.MatchString(sentence) {
		required["docker"] = true
	}
	platformText := capabilityPlatformPath.ReplaceAllStringFunc(sentence, func(reference string) string {
		if capabilityPlatformPair.MatchString(reference) {
			return reference
		}
		return " "
	})
	if capabilityWindows.MatchString(platformText) {
		required["platform"] = "windows"
	}
	if capabilityMac.MatchString(platformText) {
		required["platform"] = "darwin"
	}
	if capabilityDevice.MatchString(sentence) {
		required["realDevice"] = true
	}
	if capabilityLive.MatchString(sentence) {
		required["liveMeasurement"] = true
	}
	return required
}

// capabilityShortfall names the hosts whose declared capabilities fail the requirement. It
// is empty when every host is unknown or one host may meet it.
func capabilityShortfall(required map[string]any, hosts []EnvironmentHost) string {
	var failures []string
	for _, host := range hosts {
		values := host.Values
		if required["platform"] == "darwin" {
			if native, ok := values["nativeTest"].(map[string]any); ok {
				values = native
			}
		}
		var missing []string
		unknown := false
		for key, want := range required {
			got, ok := values[key]
			if !ok || got == nil {
				unknown = true
				continue
			}
			// A boolean true does not identify which device or measurement
			// target is available, so it cannot establish a match.
			if (key == "realDevice" || key == "liveMeasurement") && got == true {
				unknown = true
				continue
			}
			if got != want {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 && unknown {
			return ""
		}
		if len(missing) != 0 {
			sort.Strings(missing)
			failures = append(failures, host.Name+":"+strings.Join(missing, ","))
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "担当PCの能力不足（一部または全部; fleet.json / assignments.json: " + strings.Join(failures, "; ") + "）"
}

func (s *Service) environmentFindings(ctx context.Context, draft, answer Record) []environmentFinding {
	answer = executableAcceptanceAnswer(answer)
	findings := environmentRequests(answer)
	if answer["ready"] != true || s.options.Environment == nil {
		return findings
	}
	environments := make(map[string][]EnvironmentHost)
	scan := func(repo, location, value string) {
		if repo == "" {
			return
		}
		var unavailable bool
		findings = append(findings, environmentScanWith(location, value, func(sentence string) string {
			if unavailable {
				return ""
			}
			required := requiredCapabilities(sentence)
			if len(required) == 0 {
				return ""
			}
			hosts, loaded := environments[repo]
			if !loaded {
				var err error
				hosts, err = s.options.Environment(ctx, repo)
				if err != nil {
					// A later field can retry the lookup after a transient error.
					unavailable = true
					return ""
				}
				environments[repo] = hosts
			}
			kind := capabilityShortfall(required, hosts)
			if kind != "" {
				return fmt.Sprintf("%s 対象リポジトリ=%s", kind, repo)
			}
			return ""
		})...)
	}
	repo := fleet.Text(answer["project"])
	if repo == "" {
		repo = fleet.Text(draft["project"])
	}
	scan(repo, "body", fleet.Text(answer["body"]))
	for i, item := range fleet.Elements(answer["splitIssues"]) {
		child := fleet.Object(item)
		target := fleet.Text(child["project"])
		if target == "" {
			target = repo
		}
		for _, key := range []string{"title", "purpose"} {
			scan(target, fmt.Sprintf("splitIssues[%d].%s", i+1, key), fleet.Text(child[key]))
		}
		for j, item := range fleet.Elements(child["acceptance"]) {
			scan(target, fmt.Sprintf("splitIssues[%d].acceptance[%d]", i+1, j+1), fleet.Text(item))
		}
	}
	return findings
}
