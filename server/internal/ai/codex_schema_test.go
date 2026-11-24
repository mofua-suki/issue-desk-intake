package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestCodexSchemaTransportKeepsLocalUniqueValidation(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		valid        bool
	}{
		{"distinct", `{"uniqueItems":[1,2]}`, true},
		{"duplicate", `{"uniqueItems":[1,1]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := testRequest(t, "codex")
			source := []byte(`{"type":"object","additionalProperties":false,"required":["uniqueItems"],"properties":{"uniqueItems":{"anyOf":[{"type":"array","items":{"type":"integer"},"uniqueItems":true},{"type":"null"}]}}}`)
			if err := os.WriteFile(request.SchemaFile, source, 0600); err != nil {
				t.Fatal(err)
			}
			var transport string
			calls := 0
			runner := Runner{Execute: func(_ context.Context, r ProcessRequest) (ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				output := ""
				for i, arg := range r.Arguments {
					if arg == "--output-schema" {
						transport = r.Arguments[i+1]
					}
					if arg == "-o" {
						output = r.Arguments[i+1]
					}
				}
				raw, err := os.ReadFile(transport)
				if err != nil {
					t.Fatal(err)
				}
				var schema map[string]any
				if err = json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
				props := schema["properties"].(map[string]any)
				array := props["uniqueItems"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
				if _, found := array["uniqueItems"]; found {
					t.Fatal("provider rejected unsupported uniqueItems")
				}
				if _, found := props["uniqueItems"]; !found {
					t.Fatal("property was removed")
				}
				if transport == request.SchemaFile {
					t.Fatal("original schema used for transport")
				}
				if err = os.WriteFile(output, []byte(tc.answer), 0600); err != nil {
					t.Fatal(err)
				}
				return ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":1}}`)}, nil
			}}
			_, err := runner.Call(context.Background(), request)
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			var failure *Failure
			if !tc.valid && (!errors.As(err, &failure) || failure.Kind != "format") {
				t.Fatalf("duplicate accepted: %v", err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			raw, readErr := os.ReadFile(request.SchemaFile)
			if readErr != nil || string(raw) != string(source) {
				t.Fatal("original schema changed")
			}
			if _, statErr := os.Stat(transport); !os.IsNotExist(statErr) {
				t.Fatal("transport schema left behind")
			}
		})
	}
}
