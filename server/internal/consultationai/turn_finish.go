package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
	"issue-desk-intake/server/internal/workplan"
)

func (s *Service) repair(ctx context.Context, d Record, config map[string]any, used map[string]int, kind string) (bool, error) {
	value := fleet.Object(d["formatRepairs"])
	if value == nil {
		value = Record{}
	}
	kinds := fleet.Object(value["kinds"])
	if kinds == nil {
		kinds = Record{}
	}
	total := fleet.Number(value["total"])
	sum := 0.0
	executionTotal := 0.0
	for name, count := range kinds {
		n := fleet.Number(count)
		if n > 0 && n == float64(int(n)) {
			sum += n
			if name == "execution_plan" {
				executionTotal = n
			}
		}
	}
	if total < sum {
		total = sum
	}
	limit := 8.0
	if n, ok := fleet.Object(config["intake"])["formatRepairLimit"]; ok {
		limit = fleet.Number(n)
	}
	if limit < 0 || limit > 50 || limit != float64(int(limit)) {
		return false, errors.New("形式修復の上限が不正です")
	}
	if (kind == "execution_plan" && float64(used[kind]) >= limit) ||
		(kind != "execution_plan" && (used[kind] > 0 || total-executionTotal >= limit)) {
		return false, nil
	}
	used[kind]++
	kinds[kind] = fleet.Number(kinds[kind]) + 1
	return true, s.save(ctx, d, Record{"formatRepairs": Record{"total": total + 1, "kinds": kinds}})
}

type scopeFormatError struct{ error }

func (s *Service) finalize(ctx context.Context, d, answer Record, m *materials) error {
	if value, supplied := answer["executionPlan"]; supplied {
		plan, err := workplan.Parse(value)
		if err != nil {
			return err
		}
		if answer["ready"] == true && plan == nil {
			return errors.New("確定する受入条件の実行主体と検証方法がありません")
		}
		if plan != nil {
			answer["executionPlan"] = plan.Record()
		}
	}
	if answer["ready"] == true && (strings.TrimSpace(fleet.Text(answer["title"])) == "" || strings.TrimSpace(fleet.Text(answer["body"])) == "" || len(fleet.Elements(answer["questions"])) > 0) {
		answer["ready"] = false
	}
	if scope := fleet.Object(answer["changeScope"]); scope != nil {
		if answer["ready"] != true && scope["known"] == true && len(fleet.Elements(scope["paths"])) == 0 {
			scope["known"] = false
		}
		if err := validateScope(scope); err != nil {
			return &scopeFormatError{err}
		}
	}
	contextValue := Record{"projects": Record{}}
	projects := fleet.Object(contextValue["projects"])
	for repo, p := range m.state.Projects {
		value := Record{"branch": p.Branch, "revision": p.Revision, "files": []any{}, "parts": []any{}, "refused": []any{}, "references": []any{}}
		for _, doc := range p.Reads {
			record := Record{"path": doc.Path, "sha256": doc.SHA256, "bytes": doc.Size}
			if doc.Reason != "" {
				record["reason"] = doc.Reason
				value["refused"] = append(fleet.Elements(value["refused"]), record)
			} else if referenceNames[doc.Path] || doc.Path == "repo-index" || doc.Path == "repo-search" {
				record["version"] = doc.Version
				value["references"] = append(fleet.Elements(value["references"]), record)
			} else if doc.Request.Mode == "full" {
				value["files"] = append(fleet.Elements(value["files"]), record)
			} else {
				record["kind"], record["start"], record["end"], record["keyword"], record["context"] = doc.Request.Mode, doc.Request.Start, doc.Request.End, doc.Request.Keyword, doc.Request.Context
				value["parts"] = append(fleet.Elements(value["parts"]), record)
			}
		}
		if repo == fleet.Text(d["project"]) {
			for key, value := range value {
				contextValue[key] = value
			}
		} else {
			projects[repo] = value
		}
	}
	answer["context"] = contextValue
	return s.save(ctx, d, Record{"context": contextValue})
}

var namedFiles = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.[A-Za-z0-9]+|(?:AGENTS|README)\.md|(?:package|pyproject|go)\.(?:json|toml|mod)`)

func validateParts(parts []string, source string) error {
	if len(parts) < 2 || len(parts) > 25 {
		return errors.New("子の相談は2〜25件です")
	}
	original := map[string]bool{}
	for _, file := range namedFiles.FindAllString(source, -1) {
		original[file] = true
	}
	covered := map[string]bool{}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || contract.UTF16Length(part) > 8000 || len(part) >= len(source) {
			return errors.New("子の相談は元の依頼より小さい本文にしてください")
		}
		for _, file := range namedFiles.FindAllString(part, -1) {
			if !original[file] {
				return errors.New("子の相談に元の依頼にない対象があります")
			}
			covered[file] = true
		}
	}
	for file := range original {
		if !covered[file] {
			return fmt.Errorf("子の相談から対象が漏れています: %s", file)
		}
	}
	return nil
}
func (s *Service) splitInput(ctx context.Context, d Record, config map[string]any, settings models.Settings, cause error) (Record, error) {
	if s.options.Split == nil || fleet.Number(d["consultationDepth"]) >= 6 || inputSplitProhibited(d) {
		return nil, cause
	}
	source := strings.Builder{}
	for _, item := range fleet.Elements(d["messages"]) {
		message := fleet.Object(item)
		source.WriteString("[" + fleet.Text(message["role"]) + "]\n" + fleet.Text(message["text"]) + "\n\n")
	}
	if body := fleet.Text(d["body"]); body != "" && !strings.Contains(source.String(), body) {
		source.WriteString("現在の目的・決定・受入条件:\n" + body)
	}
	const schema = `{"type":"object","additionalProperties":false,"required":["parts"],"properties":{"parts":{"type":"array","minItems":2,"maxItems":25,"items":{"type":"string"}}}}`
	id, err := newID()
	if err != nil {
		return nil, err
	}
	file, err := s.writeAnswerSchema(id, schema)
	if err != nil {
		return nil, err
	}
	defer os.Remove(file)
	input := answerInput("", Record{"input": "資料本文を読まず、依頼を対象の近い子の相談へ分けてください。元の対象と要件・共通目的・利用者の決定・受入条件を漏らさず保ち、対象を増やしません。未承認のAI提案を利用者の決定へ変えません。各本文は元より小さく8000文字以内。対象ファイルは4件を目安とし、件数だけを理由に対象を省略しません。確認済みとは書きません。\n" + source.String()}, nil)
	// A new one-shot call shares the draft's budget and recovery keys, but
	// does not reuse the consultation conversation or its attached sources.
	answer, err := s.call(ctx, d, config, settings, usage.DraftStep, file, input, input, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	parts := []string{}
	for _, part := range fleet.Elements(answer.Value["parts"]) {
		parts = append(parts, fleet.Text(part))
	}
	if err := validateParts(parts, source.String()); err != nil {
		return nil, cause
	}
	return s.options.Split(ctx, d, parts)
}
