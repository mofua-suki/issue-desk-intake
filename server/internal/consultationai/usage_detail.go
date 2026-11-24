package consultationai

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/usage"
)

var usageURLPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// Legacy context restores the same repository file with an unqualified request
// path, while the original read can use the repository-qualified spelling.
func initialDocument(sources map[string]bool, doc Document) bool {
	if sources[documentKey(doc)] {
		return true
	}
	prefix := doc.Project + ":"
	if strings.HasPrefix(doc.Request.File, prefix) {
		doc.Request.File = strings.TrimPrefix(doc.Request.File, prefix)
	} else {
		doc.Request.File = prefix + doc.Request.File
	}
	return sources[documentKey(doc)]
}

// measureInput records sizes and paths only. Raw prompt and document text are never returned.
func measureInput(request ai.Request, u *ai.Usage, sources map[string]bool) {
	u.ContextReads = nil
	parts := usage.PromptPartNames()
	u.PromptBytes = make(map[string]*int64, len(parts))
	u.MeasurementMissingReasons = map[string]string{}
	for _, part := range parts {
		u.PromptBytes[part] = nil
		u.MeasurementMissingReasons[part] = "input_unavailable"
	}
	// 解析が途中で失敗した場合も、部分的な値を実測値として残さない。
	complete := false
	defer func() {
		if !complete {
			for _, part := range parts {
				u.PromptBytes[part] = nil
				u.MeasurementMissingReasons[part] = "input_unavailable"
			}
			u.ContextReads = nil
			u.MeasurementMissingReasons["contextReads"] = "paths_unavailable"
		}
	}()
	set := func(key string, n int) {
		v := int64(n)
		u.PromptBytes[key] = &v
		delete(u.MeasurementMissingReasons, key)
	}
	total := int64(len(request.Input))
	u.PromptTotalBytes = &total
	markProviderMeasurements(u)
	u.MeasurementMissingReasons["skill"] = "not_observed"
	u.MeasurementMissingReasons["environment"] = "not_observed"
	if schema, err := os.ReadFile(request.SchemaFile); err == nil {
		set("schema", len(schema))
	} else {
		u.MeasurementMissingReasons["schema"] = "schema_unavailable"
	}
	u.MeasurementMissingReasons["contextReads"] = "paths_unavailable"
	const divider = "\n以下は相談と資料のデータです:\n"
	prefix := ""
	const resumedPrefix = "同じ相談の続きです。固定指示と既送信資料を保ち、次の追加データと最新値だけを適用してください。\n"
	resumed := strings.HasPrefix(request.Input, resumedPrefix)
	if strings.HasPrefix(request.Input, turnInstructions+divider) {
		prefix = turnInstructions + divider
	} else if strings.HasPrefix(request.Input, instructions+divider) {
		prefix = instructions + divider
	} else if resumed {
		prefix = resumedPrefix
	} else {
		return
	}
	set("instructions", len(prefix))
	var data map[string]json.RawMessage
	if json.NewDecoder(strings.NewReader(strings.TrimPrefix(request.Input, prefix))).Decode(&data) != nil {
		return
	}
	measured := len(prefix)
	if raw, ok := data["skill"]; ok {
		set("skill", len(raw))
		measured += len(raw)
	}
	if !resumed {
		environment := 0
		for _, key := range []string{"project", "provider", "tier", "consultationBudget", "remainingReadRounds"} {
			environment += len(data[key])
		}
		set("environment", environment)
		measured += environment
	}
	for _, part := range []string{"references", "notes"} {
		if raw, ok := data[part]; ok {
			set(part, len(raw))
			measured += len(raw)
		}
	}
	if resumed {
		data["images"] = json.RawMessage("[]")
	}
	for part, field := range map[string]string{"conversation": "messages", "previousDraft": "current", "images": "images"} {
		raw, ok := data[field]
		if !ok {
			continue
		}
		if resumed && part == "images" {
			set(part, 0)
			continue
		}
		set(part, len(raw))
		measured += len(raw)
	}
	if len(request.Images) > 0 {
		u.PromptBytes["images"] = nil
		u.MeasurementMissingReasons["images"] = "images_unavailable"
	}
	var context struct {
		Projects []measuredProject `json:"projects"`
	}
	var documentArrays []json.RawMessage
	if resumed {
		var docs []measuredDocument
		if json.Unmarshal(data["documents"], &docs) != nil {
			return
		}
		if raw, ok := data["unreadFiles"]; ok {
			documentArrays = append(documentArrays, raw)
		}
		context.Projects = append(context.Projects, measuredProject{Documents: docs})
		documentArrays = append(documentArrays, data["documents"])
	} else if json.Unmarshal(data["context"], &context) != nil || context.Projects == nil {
		return
	} else {
		var rawContext struct {
			Projects []struct {
				Documents   json.RawMessage `json:"documents"`
				UnreadFiles json.RawMessage `json:"unreadFiles"`
			} `json:"projects"`
		}
		if json.Unmarshal(data["context"], &rawContext) != nil {
			return
		}
		for _, project := range rawContext.Projects {
			documentArrays = append(documentArrays, project.Documents, project.UnreadFiles)
		}
	}
	reads := listContextReads(context.Projects)
	set("fileList", reads.fileBytes)
	measured += reads.fileBytes
	files, requested, repeated, fileBytes, pathOK := reads.files, reads.requested, reads.repeated, reads.fileBytes, reads.pathOK
	known := sources != nil
	initial, additional, docBytes, ok := measureDocuments(documentArrays, sources)
	if !ok {
		return
	}
	measured += docBytes
	if !resumed {
		envelope := len(data["context"]) - fileBytes - docBytes
		set("contextEnvelope", envelope)
		measured += envelope
	}
	set("contextOther", len(request.Input)-measured)
	if docBytes == 0 && requested == 0 && pathOK {
		set("initialDocuments", 0)
		set("additionalDocuments", 0)
	} else if known {
		set("initialDocuments", initial)
		set("additionalDocuments", additional)
	} else {
		u.MeasurementMissingReasons["initialDocuments"] = "documents_source_unknown"
		u.MeasurementMissingReasons["additionalDocuments"] = "documents_source_unknown"
	}
	// contextReads.requested is the document count; contextReads.files contains
	// the repo/path pairs required by contextReadPaths, with duplicate counts.
	if pathOK && len(files) <= 10000 {
		u.ContextReads = map[string]any{"requested": requested, "repeated": repeated, "files": files}
		delete(u.MeasurementMissingReasons, "contextReads")
	}
	complete = true
}

// measuredDocument is the location of one document in the prompt.
type measuredDocument struct {
	Project string `json:"project"`
	Path    string `json:"path"`
}

// measuredProject is the part of one project in the prompt that measureInput counts.
type measuredProject struct {
	Name           string             `json:"name"`
	Documents      []measuredDocument `json:"documents"`
	AvailableFiles json.RawMessage    `json:"availableFiles"`
}

// contextReads counts the documents the prompt delivered, by repository and path.
type contextReads struct {
	files               []any
	requested, repeated int
	// fileBytes is the size of the available file lists.
	fileBytes int
	// pathOK is false when a document path cannot be reported.
	pathOK bool
}

// listContextReads counts the delivered documents per repository and path, with the
// repeated deliveries of the same path.
func listContextReads(projects []measuredProject) contextReads {
	reads := contextReads{files: []any{}, pathOK: true}
	seen := map[string]map[string]any{}
	for _, project := range projects {
		reads.fileBytes += len(project.AvailableFiles)
		if project.Documents == nil {
			reads.pathOK = false
			continue
		}
		for _, doc := range project.Documents {
			repo := doc.Project
			if repo == "" {
				repo = project.Name
			}
			if !reportablePath(repo, doc.Path) {
				reads.pathOK = false
				continue
			}
			reads.requested++
			key := repo + "\x00" + doc.Path
			if prior := seen[key]; prior != nil {
				prior["requested"] = prior["requested"].(int) + 1
				prior["repeated"] = prior["repeated"].(int) + 1
				reads.repeated++
			} else {
				file := map[string]any{"repo": repo, "path": doc.Path, "requested": 1, "repeated": 0}
				seen[key] = file
				reads.files = append(reads.files, file)
			}
		}
	}
	return reads
}

// reportablePath tells whether a repository and path can be reported without carrying a URL
// or a line break.
func reportablePath(repo, path string) bool {
	return strings.TrimSpace(repo) != "" && strings.TrimSpace(path) != "" && contract.UTF16Length(repo) <= 200 && contract.UTF16Length(path) <= 200 && !strings.ContainsAny(repo+path, "\r\n\u2028\u2029") && !usageURLPattern.MatchString(repo) && !usageURLPattern.MatchString(path) && !strings.Contains(repo, "://") && !strings.Contains(path, "://")
}

// measureDocuments sums the document sizes, separating the documents of the initial read
// from the additional ones. ok is false when a document cannot be parsed.
func measureDocuments(arrays []json.RawMessage, sources map[string]bool) (initial, additional, total int, ok bool) {
	known := sources != nil
	for _, array := range arrays {
		if len(array) == 0 {
			continue
		}
		var docs []json.RawMessage
		if json.Unmarshal(array, &docs) != nil {
			return 0, 0, 0, false
		}
		for _, raw := range docs {
			var doc Document
			if json.Unmarshal(raw, &doc) != nil {
				return 0, 0, 0, false
			}
			total += len(raw)
			if known && initialDocument(sources, doc) {
				initial += len(raw)
			} else {
				additional += len(raw)
			}
		}
	}
	return initial, additional, total, true
}

// markProviderMeasurements marks the call and context token counts the provider did not
// report, and drops the counts of a partial measurement.
func markProviderMeasurements(u *ai.Usage) {
	if u.Calls == nil {
		u.MeasurementMissingReasons["calls"] = "provider_unavailable"
	}
	if u.ContextMaxTokens == nil {
		u.MeasurementMissingReasons["contextMaxTokens"] = "provider_unavailable"
	}
	if u.ContextTotalTokens == nil {
		u.MeasurementMissingReasons["contextTotalTokens"] = "provider_unavailable"
	}
	if u.Partial != nil && *u.Partial {
		u.Calls = nil
		u.ContextMaxTokens = nil
		u.ContextTotalTokens = nil
		u.MeasurementMissingReasons["calls"] = "partial"
		u.MeasurementMissingReasons["contextMaxTokens"] = "partial"
		u.MeasurementMissingReasons["contextTotalTokens"] = "partial"
	}
}
