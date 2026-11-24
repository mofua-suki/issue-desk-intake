package consultationai

import (
	"encoding/base64"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	imagefiles "issue-desk-intake/server/internal/images"
	"os"
	"path/filepath"
)

func (s *Service) answerImages(request map[string]any) ([]string, []Record, func(), error) {
	entries := fleet.Elements(request["images"])
	if len(entries) == 0 {
		return nil, nil, func() {}, nil
	}
	if request["kind"] != "image-comment" || len(entries) > 4 {
		return nil, nil, nil, errors.New("返信画像は4枚以内です")
	}
	workspace := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, nil, nil, err
	}
	dir, err := os.MkdirTemp(workspace, ".intake-images-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	paths := []string{}
	metadata := []Record{}
	seen := map[string]bool{}
	for index, item := range entries {
		image := fleet.Object(item)
		id := fleet.Text(image["id"])
		data := fleet.Text(image["data"])
		if !uuid.MatchString(id) || seen[id] || len(data) > 3<<20 {
			cleanup()
			return nil, nil, nil, errors.New("返信画像のID・大きさが不正です")
		}
		seen[id] = true
		raw, err := base64.StdEncoding.Strict().DecodeString(data)
		if err != nil || base64.StdEncoding.EncodeToString(raw) != data {
			cleanup()
			return nil, nil, nil, errors.New("返信画像の形式が不正です")
		}
		info, err := imagefiles.Inspect(raw)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		if fleet.Text(image["mime"]) != info.MIME {
			cleanup()
			return nil, nil, nil, errors.New("返信画像のMIMEが内容と一致しません")
		}
		file := filepath.Join(dir, fmt.Sprintf("%d.%s", index, info.Ext))
		if err := os.WriteFile(file, raw, 0600); err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		paths = append(paths, file)
		metadata = append(metadata, Record{"id": id, "mime": info.MIME, "sha256": info.SHA256, "width": info.Width, "height": info.Height, "file": file})
	}
	return paths, metadata, cleanup, nil
}
