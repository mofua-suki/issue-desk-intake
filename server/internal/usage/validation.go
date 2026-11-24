package usage

import (
	_ "embed"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

//go:embed schema.json
var schemaJSON []byte

// MaxMeasurementMissingReasons is the maximum number of missing measurement reasons in a usage row.
const MaxMeasurementMissingReasons = 20

var schema = func() map[string][]string {
	var value map[string][]string
	if json.Unmarshal(schemaJSON, &value) != nil {
		panic("invalid usage schema")
	}
	return value
}()

// RecordedRepairKind reports whether value is a format repair kind stored in usage rows.
func RecordedRepairKind(value any) bool {
	return oneOf(value, schema["recordedRepairKinds"]...)
}

// RecordedRepairKinds returns the format repair kinds stored in usage rows.
func RecordedRepairKinds() []string {
	return append([]string(nil), schema["recordedRepairKinds"]...)
}

// PromptPartNames returns the prompt parts accepted in persisted usage rows.
func PromptPartNames() []string {
	return append([]string(nil), schema["promptPartNames"]...)
}

func oneOf(value any, allowed ...string) bool {
	v, ok := value.(string)
	if !ok {
		return false
	}
	for _, item := range allowed {
		if v == item {
			return true
		}
	}
	return false
}

// ValidDiagnosticKind uses the same allowlist as persisted intake usage rows.
func ValidDiagnosticKind(value string) bool {
	return oneOf(value, schema["diagnosticKinds"]...)
}
func fields(value map[string]any, allowed []string) bool {
	for key := range value {
		if !oneOf(key, allowed...) {
			return false
		}
	}
	return true
}
func count(value any, min float64) bool {
	v, ok := value.(float64)
	return ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= min && v <= 9007199254740991 && v == math.Trunc(v)
}
func amount(value any) bool {
	v, ok := value.(float64)
	return ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}
func text(value any, max int) bool {
	v, ok := value.(string)
	return ok && strings.TrimSpace(v) != "" && (max == 0 || contract.UTF16Length(v) <= max)
}
func atTime(value any) bool {
	v, ok := value.(string)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, v)
	return err == nil && contract.ISO(at) == v
}

func ValidUsage(row map[string]any) bool {
	if row == nil || !fields(row, schema["usageFields"]) || !text(row["host"], 0) || !atTime(row["at"]) || !oneOf(row["source"], schema["usageSources"]...) {
		return false
	}
	textFields := strings.Fields("repo draft step provider model effort tier")
	countFields := strings.Fields("attempt generation turn elapsedMs inputTokens outputTokens cacheReadTokens cacheWriteTokens reviewDiffFiles reviewDiffLines calls contextMaxTokens contextTotalTokens promptTotalBytes")
	enums := map[string]string{"kind": "usageKinds", "outcome": "usageOutcomes", "costUnknownReason": "costUnknownReasons", "generationTrigger": "generationTriggers", "workType": "workTypes", "billing": "billingValues"}
	for key, value := range row {
		if key == "callId" {
			id, ok := value.(string)
			if row["source"] != "intake" || !ok || !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(id) {
				return false
			}
		}
		if key == "diagnosticKind" && (row["source"] != "intake" || !oneOf(value, schema["diagnosticKinds"]...)) {
			return false
		}
		// An absent measurement is allowed; null is not a measured duration.
		if key == "firstOutputMs" && (row["source"] != "intake" || !count(value, 0)) {
			return false
		}
		if value == nil {
			if key == "kind" {
				return false
			}
			continue
		}
		if oneOf(key, textFields...) && !text(value, 0) {
			return false
		}
		if oneOf(key, countFields...) && !count(value, 0) {
			return false
		}
		if group, exists := enums[key]; exists && !oneOf(value, schema[group]...) {
			return false
		}
		switch key {
		case "issue":
			if !count(value, 1) {
				return false
			}
		case "costUsd", "sessionCostUsd":
			if !amount(value) {
				return false
			}
		case "reviewSmallChange", "usagePartial":
			if _, ok := value.(bool); !ok {
				return false
			}
		case "serviceTier", "fastMode":
			if !text(value, 200) {
				return false
			}
		case "promptCache":
			if value != false {
				return false
			}
		case "dispatchGeneration", "deskGeneration":
			if row["source"] != "worker" || !count(value, 0) {
				return false
			}
		case "leaseEpoch":
			if row["source"] != "worker" || !count(value, 1) {
				return false
			}
		case "modelCalls":
			if row["provider"] != "codex" || !count(value, 1) {
				return false
			}
		case "sessionMode":
			if !oneOf(value, "new", "resumed", "once") {
				return false
			}
		case "sessionCall":
			if !oneOf(row["sessionMode"], "new", "resumed") || !count(value, 1) {
				return false
			}
		case "usageMissingReason":
			if !oneOf(value, "execution-error", "usage-unavailable") {
				return false
			}
		case "callTrigger":
			if row["source"] != "intake" || !IsIntakeStep(fleet.Text(row["step"])) || !oneOf(value, schema["intakeCallTriggers"]...) {
				return false
			}
		case "repairKind":
			if row["callTrigger"] != "format_repair" || !RecordedRepairKind(value) {
				return false
			}
		case "repairField":
			if row["repairKind"] != "draft_format" || !text(value, 80) || !regexp.MustCompile(`^[A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*$`).MatchString(fleet.Text(value)) {
				return false
			}
		case "reviewFindingKinds":
			list, ok := value.([]any)
			if !ok {
				return false
			}
			for _, v := range list {
				if !oneOf(v, schema["reworkFindingKinds"]...) {
					return false
				}
			}
		case "reviewFindingSeverities":
			list, ok := value.([]any)
			if !ok {
				return false
			}
			for _, v := range list {
				object := fleet.Object(v)
				if object == nil || len(object) != 2 || !oneOf(object["kind"], schema["reworkFindingKinds"]...) || !oneOf(object["severity"], "must", "minor") {
					return false
				}
			}
		case "tools", "toolErrors", "missingCommands", "promptBytes":
			object := fleet.Object(value)
			if object == nil {
				return false
			}
			if key == "tools" && len(object) > 50 || key == "missingCommands" && len(object) > 20 {
				return false
			}
			for name, n := range object {
				if key == "promptBytes" && n == nil {
					if row["source"] != "intake" || !oneOf(name, schema["promptPartNames"]...) {
						return false
					}
					continue
				}
				if !text(name, 200) || !count(n, 0) {
					return false
				}
				if key == "toolErrors" && !oneOf(name, schema["toolErrorClasses"]...) {
					return false
				}
				if key == "promptBytes" && !oneOf(name, schema["promptPartNames"]...) {
					return false
				}
				if key == "missingCommands" && name != "?" && !regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`).MatchString(name) {
					return false
				}
			}
		case "measurementMissingReasons":
			object := fleet.Object(value)
			if row["source"] != "intake" || object == nil || len(object) > MaxMeasurementMissingReasons {
				return false
			}
			for name, reason := range object {
				if !(oneOf(name, schema["promptPartNames"]...) || oneOf(name, "calls", "contextMaxTokens", "contextTotalTokens", "promptTotalBytes", "contextReads")) || !oneOf(reason, "provider_unavailable", "partial", "documents_source_unknown", "input_unavailable", "schema_unavailable", "images_unavailable", "not_observed", "paths_unavailable") {
					return false
				}
			}
		case "files":
			list, ok := value.([]any)
			if !ok || len(list) > 20 {
				return false
			}
			for _, v := range list {
				file := fleet.Object(v)
				if file == nil || len(file) != 2 || !fields(file, []string{"path", "bytes"}) || !text(file["path"], 500) || file["bytes"] != nil && !count(file["bytes"], 0) {
					return false
				}
			}
		case "modelUsage":
			models := fleet.Object(value)
			if models == nil || len(models) > 20 {
				return false
			}
			for model, v := range models {
				object := fleet.Object(v)
				if !text(model, 200) || object == nil || len(object) != 5 || !fields(object, strings.Fields("inputTokens outputTokens cacheReadTokens cacheWriteTokens costUsd")) {
					return false
				}
				for name, n := range object {
					if n != nil && (name == "costUsd" && !amount(n) || name != "costUsd" && !count(n, 0)) {
						return false
					}
				}
			}
		case "rateLimit":
			if !validRateLimit(fleet.Object(value)) {
				return false
			}
		case "contextReads":
			if !validContextReads(row, fleet.Object(value)) {
				return false
			}
		}
	}
	return true
}

func validRateLimit(value map[string]any) bool {
	if value == nil || len(value) != 6 || !fields(value, strings.Fields("status type utilization resetsAt isUsingOverage windows")) {
		return false
	}
	for _, key := range []string{"status", "type"} {
		if value[key] != nil && !text(value[key], 0) {
			return false
		}
	}
	if value["utilization"] != nil && !amount(value["utilization"]) {
		return false
	}
	if value["resetsAt"] != nil && !atTime(value["resetsAt"]) {
		return false
	}
	if value["isUsingOverage"] != nil {
		if _, ok := value["isUsingOverage"].(bool); !ok {
			return false
		}
	}
	if value["windows"] != nil {
		windows := fleet.Object(value["windows"])
		if windows == nil || len(windows) > 20 {
			return false
		}
		for name, item := range windows {
			window := fleet.Object(item)
			if !text(name, 200) || window == nil || len(window) != 2 || !fields(window, []string{"utilization", "resetsAt"}) || window["utilization"] != nil && !amount(window["utilization"]) || window["resetsAt"] != nil && !atTime(window["resetsAt"]) {
				return false
			}
		}
	}
	return true
}

func validContextReads(row, value map[string]any) bool {
	if row["source"] != "intake" || value == nil || (len(value) != 3 && len(value) != 4) || !fields(value, schema["contextReadsFields"]) || !count(value["requested"], 0) || !count(value["repeated"], 0) {
		return false
	}
	if raw, exists := value["deliveryBudget"]; exists {
		budget := fleet.Object(raw)
		if budget == nil || len(budget) != 4 || !fields(budget, schema["deliveryBudgetFields"]) || !count(budget["round"], 0) || !count(budget["omitted"], 0) || !count(budget["deliveredBytes"], 0) || budget["deliveredBytes"].(float64) > 160000 {
			return false
		}
		if _, ok := budget["limited"].(bool); !ok {
			return false
		}
	}
	files, ok := value["files"].([]any)
	if !ok || len(files) > 10000 {
		return false
	}
	for _, item := range files {
		file := fleet.Object(item)
		if file == nil || len(file) != 4 || !fields(file, []string{"repo", "path", "requested", "repeated"}) || !text(file["repo"], 200) || !text(file["path"], 200) || !count(file["requested"], 0) || !count(file["repeated"], 0) {
			return false
		}
		for _, key := range []string{"repo", "path"} {
			v := fleet.Text(file[key])
			if strings.ContainsAny(v, "\r\n\u2028\u2029") || regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(v) || strings.Contains(v, "://") {
				return false
			}
		}
	}
	return true
}

func ValidLimit(row map[string]any) bool {
	if value, exists := row["provider"]; exists && value == nil {
		return false
	}
	if row == nil || !fields(row, schema["limitFields"]) || !text(row["host"], 0) || !atTime(row["at"]) || !oneOf(row["source"], schema["limitSources"]...) {
		return false
	}
	for key, value := range row {
		if value == nil {
			continue
		}
		switch key {
		case "provider":
			if !oneOf(value, schema["limitProviders"]...) {
				return false
			}
		case "limit", "status":
			if !text(value, 0) {
				return false
			}
		case "utilization", "percent":
			v, ok := value.(float64)
			if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
				return false
			}
		case "windowMinutes":
			if !count(value, 1) {
				return false
			}
		case "resetsAt":
			if !atTime(value) {
				return false
			}
		}
	}
	return true
}
