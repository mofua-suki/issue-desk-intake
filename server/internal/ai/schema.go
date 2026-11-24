package ai

import (
	"bytes"
	"errors"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type localOnly struct{}

func (localOnly) Load(string) (any, error) {
	return nil, errors.New("外部のスキーマ参照は許可していません")
}

func Compile(file string) (*jsonschema.Schema, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("AI回答のスキーマを読み取れません")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("AI回答のスキーマが不正です")
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(localOnly{})
	const name = "urn:issue-desk:response"
	if err := compiler.AddResource(name, document); err != nil {
		return nil, errors.New("AI回答のスキーマを登録できません")
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		return nil, errors.New("AI回答のスキーマを検証できません")
	}
	return schema, nil
}

func Validate(schema *jsonschema.Schema, data []byte) (map[string]any, error) {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("AIの回答をJSONとして読み取れません")
	}
	if schema == nil || schema.Validate(value) != nil {
		return nil, errors.New("AIの回答が指定の形式に一致しません")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("AIの回答がオブジェクトではありません")
	}
	return object, nil
}
