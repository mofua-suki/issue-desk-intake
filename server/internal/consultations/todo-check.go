package consultations

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
)

var invalidPath = regexp.MustCompile(`[\\:*?\[\]{}\x00-\x1f]`)
var issueMethod = regexp.MustCompile(`^(Issue完了|Issue未完了):\s*共通番号([1-9][0-9]*)$`)
var utcTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$`)

func relative(value string) error {
	if value == "" || invalidPath.MatchString(value) || strings.HasPrefix(value, "/") {
		return refuse(400, "確かめ方にはワイルドカードを含まない相対パスを指定してください。")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return refuse(400, "確かめ方のパスに..は使えません。")
		}
	}
	if path.IsAbs(value) {
		return refuse(400, "相対パスを指定してください。")
	}
	return nil
}

func parseMethod(input any) (Record, error) {
	line, ok := input.(string)
	if !ok || strings.TrimSpace(line) == "" || contract.UTF16Length(line) > 300 || strings.ContainsAny(line, "\r\n") {
		return nil, refuse(400, "確かめ方は1行300文字以内で入力してください。")
	}
	line = strings.TrimSpace(line)
	for prefix, kind := range map[string]string{"ファイルあり:": "fileExists", "ファイルなし:": "fileAbsent"} {
		if strings.HasPrefix(line, prefix) {
			p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if err := relative(p); err != nil {
				return nil, err
			}
			return Record{"kind": kind, "path": p}, nil
		}
	}
	for prefix, kind := range map[string]string{"文字あり:": "textExists", "文字なし:": "textAbsent"} {
		if strings.HasPrefix(line, prefix) {
			parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, prefix)), "::", 2)
			if len(parts) != 2 {
				return nil, refuse(400, "文字の確認はパス :: 語の形で指定してください。")
			}
			p, word := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if err := relative(p); err != nil {
				return nil, err
			}
			if word == "" || contract.UTF16Length(word) > 200 {
				return nil, refuse(400, "確かめ方の語は1〜200文字で入力してください。")
			}
			return Record{"kind": kind, "path": p, "word": word}, nil
		}
	}
	if m := issueMethod.FindStringSubmatch(line); m != nil {
		n, err := json.Number(m[2]).Int64()
		if err == nil && n <= 9007199254740991 {
			kind := "issueCompleted"
			if m[1] == "Issue未完了" {
				kind = "issueIncomplete"
			}
			return Record{"kind": kind, "number": n}, nil
		}
	}
	if strings.HasPrefix(line, "コマンド:") {
		command := strings.TrimSpace(strings.TrimPrefix(line, "コマンド:"))
		if command != "" {
			return Record{"kind": "command", "command": command}, nil
		}
	}
	return nil, refuse(400, "確かめ方はファイル・文字・Issue・コマンドの形で入力してください。")
}

func parseCheck(input any) (any, error) {
	if input == nil {
		return nil, nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, refuse(400, "確認条件の形が不正です。")
	}
	value, err := decode(string(raw))
	if err != nil {
		return nil, refuse(400, "確認条件の形が不正です。")
	}
	for _, key := range []string{"problem", "resolved"} {
		s, ok := value[key].(string)
		if !ok || strings.TrimSpace(s) == "" || contract.UTF16Length(s) > 2000 {
			return nil, refuse(400, "問題と解決の条件はそれぞれ1〜2000文字で入力してください。")
		}
	}
	methods, ok := value["methods"].([]any)
	if !ok || len(methods) > 10 {
		return nil, refuse(400, "確かめ方は最大10行の配列で入力してください。")
	}
	parsed := make([]Record, 0, len(methods))
	for _, line := range methods {
		method, err := parseMethod(line)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, method)
	}
	return Record{"problem": value["problem"], "resolved": value["resolved"], "methods": methods, "parsedMethods": parsed}, nil
}

func same(a, b any) bool {
	first, err1 := json.Marshal(a)
	second, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(first) == string(second)
}

func todoFields(current, data Record, now time.Time) (Record, error) {
	result := Record{}
	replay := text(data["id"]) != "" && text(current["requestId"]) == text(data["id"])
	if check, ok := data["check"]; ok {
		parsed, err := parseCheck(check)
		if err != nil {
			return nil, err
		}
		result["check"] = parsed
	}
	if input, present := data["dueAt"]; present {
		due, ok := input.(string)
		if !ok || !utcTime.MatchString(due) {
			return nil, refuse(400, "期限はISO 8601のUTC文字列で入力してください。")
		}
		t, err := time.Parse(time.RFC3339Nano, due)
		if err != nil {
			return nil, refuse(400, "期限の日付が不正です。")
		}
		if !(replay && due == text(current["dueAt"])) && (t.Before(now.Add(24*time.Hour)) || t.After(now.Add(30*24*time.Hour))) {
			return nil, refuse(400, "期限は保存時刻の1〜30日先にしてください。")
		}
		result["dueAt"] = due
	} else if text(current["body"]) == "" && text(current["dueAt"]) == "" {
		result["dueAt"] = timestamp(now.Add(7 * 24 * time.Hour))
	}
	if replay {
		for key, value := range result {
			if !same(value, current[key]) {
				return nil, refuse(409, "送信IDは保存済みの内容で使用済みです。")
			}
		}
	}
	return result, nil
}
