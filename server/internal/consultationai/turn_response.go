package consultationai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

const turnResponseInstructions = `回答は必ず {"response": 回答} で包んでください。追加資料が必要な途中回答では、responseにreadFiles・escalate・escalateReasonだけを返し、下書き本文・計画・分割案を作り直さないでください。追加資料を要求しない回答は、responseに完全な回答スキーマの全項目を返してください。最終回に途中回答は使えません。`

// Build the envelope for the current phase from the authoritative complete schema.
func turnResponseSchema(runtime, source string, final bool) (string, []byte, func(), error) {
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", nil, nil, err
	}
	var complete map[string]any
	if err = json.Unmarshal(raw, &complete); err != nil {
		return "", nil, nil, err
	}
	properties := fleet.Object(complete["properties"])
	if properties["readFiles"] == nil {
		return "", nil, nil, errors.New("資料要求の回答スキーマがありません")
	}
	readFiles := fleet.Object(properties["readFiles"])
	var response Record = complete
	if final {
		readFiles["maxItems"] = 0
	} else {
		readFiles = fleet.Object(fleet.Clone(readFiles))
		readFiles["minItems"] = 1
		reading := Record{"type": "object", "additionalProperties": false,
			"required":   []string{"readFiles", "escalate", "escalateReason"},
			"properties": Record{"readFiles": readFiles, "escalate": properties["escalate"], "escalateReason": properties["escalateReason"]}}
		response = Record{"anyOf": []any{reading, complete}}
	}
	encoded, err := json.Marshal(Record{"type": "object", "additionalProperties": false,
		"required": []string{"response"}, "properties": Record{"response": response}})
	if err != nil {
		return "", nil, nil, err
	}
	if err = os.MkdirAll(runtime, 0700); err != nil {
		return "", nil, nil, err
	}
	dir, err := os.MkdirTemp(runtime, "intake-response-")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, "intake.schema.json")
	if err = os.WriteFile(file, encoded, 0600); err != nil {
		cleanup()
		return "", nil, nil, err
	}
	return file, encoded, cleanup, nil
}

func turnResponse(value map[string]any, final bool) (Record, error) {
	// Internal Caller implementations and saved answers can still use the complete
	// flat shape. The actual provider boundary validates the envelope schema.
	wrapped, exists := value["response"]
	if !exists {
		return Record(value), nil
	}
	answer := fleet.Object(wrapped)
	if answer == nil {
		return nil, &ai.Failure{Kind: "format", Message: "相談の回答がオブジェクトではありません"}
	}
	if _, complete := answer["ready"]; complete {
		return Record(answer), nil
	}
	if final || len(fleet.Elements(answer["readFiles"])) == 0 {
		return nil, &ai.Failure{Kind: "format", Message: "最終回答には完全な下書きと確認結果が必要です"}
	}
	// Never persist this branch as a ready answer, even if a custom Caller bypasses
	// transport validation. It carries no plan or approval that could replace one.
	return Record{"readFiles": answer["readFiles"], "escalate": answer["escalate"], "escalateReason": answer["escalateReason"], "ready": false, "reply": "追加資料を確認しています。"}, nil
}
