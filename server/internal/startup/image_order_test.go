package startup

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/images"
)

func TestAIImagesPreserveMessageAndAttachmentOrder(t *testing.T) {
	ctx := context.Background()
	local := newAppStore(t)
	if _, err := consultations.OpenRecords(ctx, local.DB()); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	info, err := images.Inspect(buffer.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"sha256": info.SHA256})
	for _, row := range []struct{ id, message string }{{"z-first", "m1"}, {"a-second", "m1"}, {"b-last", "m2"}} {
		if _, err = local.DB().ExecContext(ctx, "INSERT INTO images(id,scope,message,json,bytes) VALUES(?,?,?,?,?)", row.id, "draft:d", row.message, string(metadata), buffer.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	d := consultations.Record{"id": "d", "messages": []any{
		map[string]any{"id": "m1", "images": []any{map[string]any{"id": "z-first"}, map[string]any{"id": "a-second"}}},
		map[string]any{"id": "m2", "images": []any{map[string]any{"id": "b-last"}}},
	}}
	a := App{SQLite: local}
	for range 16 {
		got, err := a.aiImages(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, value := range got {
			ids = append(ids, value.ID)
		}
		if !reflect.DeepEqual(ids, []string{"z-first", "a-second", "b-last"}) {
			t.Fatal(ids)
		}
	}
}
