package consultationai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReadSchemasMatchExecutableReadRequests(t *testing.T) {
	object := func(file, mode string, keyword any) Record {
		return Record{"file": file, "mode": mode, "start": nil, "end": nil, "keyword": keyword, "context": float64(5)}
	}
	cases := []struct {
		name  string
		value any
		valid bool
	}{
		{"plain full", "README.md", true},
		{"qualified full", "owner/app:README.md", true},
		{"normal file named like reservation", "src/repo-search", true},
		{"full object", object("README.md", "full", nil), true},
		{"line object", Record{"file": "README.md", "mode": "lines", "start": float64(1), "end": float64(2), "keyword": nil, "context": float64(5)}, true},
		{"search", object("repo-search", "keyword", "Identifier"), true},
		{"qualified search", object("owner/app:repo-search", "keyword", "Identifier"), true},
		{"reserved full string", "repo-search", false},
		{"qualified reserved string", "owner/app:repo-search", false},
		{"reserved outline", object("repo-search", "outline", nil), false},
		{"reserved full object", object("repo-search", "full", nil), false},
		{"empty search", object("repo-search", "keyword", ""), false},
		{"missing search", object("repo-search", "keyword", nil), false},
	}
	for _, name := range []string{"intake.schema.json", "proxy.schema.json", "filing-scope.schema.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
			if err != nil {
				t.Fatal(err)
			}
			var document Record
			if err = json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			if err = compiler.AddResource("urn:reads", document["properties"].(map[string]any)["readFiles"]); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile("urn:reads")
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					encoded, err := json.Marshal([]any{tc.value})
					if err != nil {
						t.Fatal(err)
					}
					var instance any
					if err = json.Unmarshal(encoded, &instance); err != nil {
						t.Fatal(err)
					}
					err = schema.Validate(instance)
					if (err == nil) != tc.valid {
						t.Fatalf("schema valid=%v expected=%v: %v", err == nil, tc.valid, err)
					}
					if tc.valid {
						if _, err := request(tc.value); err != nil {
							t.Fatalf("schema accepted an unexecutable request: %v", err)
						}
					}
				})
			}
		})
	}
}
