package reviewnotes_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func TestPostgresMalformedNotesKeepsFleetAvailable(t *testing.T) {
	db, ctx := postgres(t)
	f := fleet.Service{DB: db, FleetPath: "../../../fleet.json", AssignmentsPath: "../../../assignments.json"}
	badRepo, goodRepo := "mofua-suki/issue-desk-intake", "mofua-suki/demo-image-culling"
	exec(t, db, ctx, `INSERT INTO repo_note_processing(repo,notes) VALUES($1,'{}'),($2,'{"items":[{"kind":"legacy","text":"有効な注意点"}]}')`, badRepo, goodRepo)
	for _, raw := range []string{
		`{"items":{"kind":1}}`,
		`{"items":[{"kind":1,"text":"不正"}]}`,
		`{"items":[{"kind":"bad","text":false}]}`,
		`{"items":[{"kind":"valid","text":"部分的に有効"},{"kind":1}]}`,
		`[]`,
	} {
		exec(t, db, ctx, `UPDATE repo_note_processing SET notes=$2::jsonb WHERE repo=$1`, badRepo, raw)
		body, err := f.ClientConfig(ctx, "nas", "")
		if err != nil {
			t.Fatalf("1リポジトリの不正なnotesで配信全体が失敗: %v", err)
		}
		public := fleet.Object(body["reviewNotes"])
		if !reflect.DeepEqual(public[badRepo], []map[string]string{}) || !reflect.DeepEqual(public[goodRepo], []map[string]string{{"kind": "legacy", "text": "有効な注意点"}}) || body["config"] == nil {
			t.Fatal("不正な行の隔離でほかのリポジトリ・構成を損失", public)
		}
		wire, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		items, ok := fleet.Object(decoded["reviewNotes"])[badRepo].([]any)
		if !ok || len(items) != 0 {
			t.Fatal("配信JSONの空配列を維持していない", decoded["reviewNotes"])
		}
	}
}
