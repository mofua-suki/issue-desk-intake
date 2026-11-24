package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-intake/server/internal/contract"
	imagefiles "issue-desk-intake/server/internal/images"
)

func (s *Service) imageInputs(ctx context.Context, d Record) ([]string, []Record, func(), error) {
	hasImages := false
	for _, item := range fleet.Elements(d["messages"]) {
		if len(fleet.Elements(fleet.Object(item)["images"])) > 0 {
			hasImages = true
		}
	}
	if s.options.Images == nil {
		if hasImages {
			return nil, nil, nil, errors.New("添付画像を読み取る接続がありません")
		}
		return nil, nil, func() {}, nil
	}
	images, err := s.options.Images(ctx, d)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(images) > imagefiles.PerConversation {
		return nil, nil, nil, errors.New("添付画像が相談全体の上限を超えています")
	}
	if len(images) == 0 {
		return nil, nil, func() {}, nil
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
	for index, image := range images {
		if !uuid.MatchString(image.ID) || seen[image.ID] {
			cleanup()
			return nil, nil, nil, errors.New("添付画像のIDが不正です")
		}
		seen[image.ID] = true
		info, err := imagefiles.Inspect(image.Bytes)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		if image.SHA256 != "" && image.SHA256 != info.SHA256 {
			cleanup()
			return nil, nil, nil, errors.New("保存した画像と取得した内容が一致しません")
		}
		file := filepath.Join(dir, fmt.Sprintf("%d.%s", index, info.Ext))
		if err := os.WriteFile(file, image.Bytes, 0600); err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		paths = append(paths, file)
		metadata = append(metadata, Record{"id": image.ID, "sha256": info.SHA256, "mime": info.MIME, "width": info.Width, "height": info.Height, "file": file})
	}
	return paths, metadata, cleanup, nil
}
func validateImages(images []Record, answer Record) error {
	if len(images) == 0 {
		return nil
	}
	reviews := fleet.Elements(answer["imageReviews"])
	if len(reviews) != len(images) {
		return errors.New("添付画像の確認が不足しています")
	}
	seen := map[string]bool{}
	for _, image := range images {
		var review map[string]any
		for _, item := range reviews {
			if fleet.Text(fleet.Object(item)["id"]) == fleet.Text(image["id"]) {
				if review != nil {
					return errors.New("画像の確認が重複しています")
				}
				review = fleet.Object(item)
			}
		}
		if review == nil || review["readable"] != true || seen[fleet.Text(review["id"])] {
			return errors.New("画像を読み取れたことを確認できません")
		}
		seen[fleet.Text(review["id"])] = true
		for _, key := range []string{"screen", "location", "change", "uncertainty"} {
			value, ok := review[key].(string)
			if !ok || contract.UTF16Length(value) > 2000 || key != "uncertainty" && strings.TrimSpace(value) == "" {
				return errors.New("画像の画面・箇所・変更内容の具体的な確認が必要です")
			}
		}
	}
	return nil
}
