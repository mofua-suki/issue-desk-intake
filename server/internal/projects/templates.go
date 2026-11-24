package projects

import (
	"fmt"
	"io/fs"
	"issue-desk-intake/server/internal/fleet"
	"path"
	"sort"
	"strings"
)

type ProjectFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func ProjectFiles(d Object, owner string) ([]ProjectFile, error) {
	values, layers := templateValues(d, owner)
	return renderTemplates(layers, values)
}

// templateValues はプロジェクト定義から、テンプレートへ差し込む値と使う層を決める。
func templateValues(d Object, owner string) (map[string]string, []string) {
	command := func(v any) string {
		a := []string{}
		for _, v := range fleet.List(v) {
			a = append(a, fleet.Text(v))
		}
		return strings.Join(a, " ")
	}
	setup := command(d["setupCommand"])
	setupText := "なし"
	setupLine := "- テスト環境の準備: なし"
	if setup != "" {
		setupText = "`" + setup + "`"
		setupLine = "- テスト環境の準備（固定テストの前に実行）: `" + setup + "`"
	}
	deploy := d["autoDeploy"] == true && d["placement"] == "a9max"
	rule := ""
	if deploy {
		rule = "\n- 自動デプロイ: アプリが `GET /health` で反映版を答える契約を保つ。反映の手順は反映先PCの hostd.json の apps にあり、このリポジトリには置かない。"
	}
	tech := map[string]string{"node": "Node.js 24", "python": "Python 3.12"}[fleet.Text(d["technology"])]
	placement := map[string]string{"a9max": "A9 MAX", "nas": "NAS"}[fleet.Text(d["placement"])]
	summary := ""
	for _, line := range strings.Split(fleet.Text(d["description"]), "\n") {
		if strings.TrimSpace(line) != "" {
			summary = strings.TrimSpace(line)
			break
		}
	}
	r := []rune(summary)
	if len(r) > 200 {
		summary = string(r[:200])
	}
	values := map[string]string{"name": fleet.Text(d["name"]), "owner": owner, "packageName": strings.ToLower(fleet.Text(d["name"])), "description": fleet.Text(d["description"]), "summary": summary, "technology": tech, "placement": placement, "testCommand": command(d["testCommand"]), "setupCommand": setupText, "setupLine": setupLine, "deployRule": rule}
	layers := []string{"common", fleet.Text(d["technology"])}
	if deploy {
		layers = append(layers, "deploy/"+fleet.Text(d["technology"]))
	}
	return values, layers
}

// renderTemplates は層ごとのテンプレートに値を差し込み、パスの順に並べた初期ファイルを返す。後の層が同じパスを上書きする。
func renderTemplates(layers []string, values map[string]string) ([]ProjectFile, error) {
	files := map[string]string{}
	for _, layer := range layers {
		e := fs.WalkDir(assets, "templates/"+layer, func(file string, entry fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if entry.IsDir() {
				return nil
			}
			raw, e := assets.ReadFile(file)
			if e != nil {
				return e
			}
			text := strings.ReplaceAll(string(raw), "\r\n", "\n")
			target := strings.TrimPrefix(file, "templates/"+layer+"/")
			parts := strings.Split(target, "/")
			for i, v := range parts {
				if v == "_gitignore" {
					parts[i] = ".gitignore"
				}
			}
			target = strings.Join(parts, "/")
			for k, v := range values {
				if path.Ext(file) == ".json" || path.Ext(file) == ".toml" {
					encoded := string(jsonBytes(v))
					v = strings.ReplaceAll(encoded[1:len(encoded)-1], "\x7f", `\u007f`)
				}
				text = strings.ReplaceAll(text, "{{"+k+"}}", v)
			}
			if strings.Contains(text, "{{") {
				return fmt.Errorf("unknown template placeholder in %s", file)
			}
			files[target] = text
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	keys := []string{}
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []ProjectFile{}
	for _, k := range keys {
		out = append(out, ProjectFile{k, files[k]})
	}
	return out, nil
}

func registrationEntry(d Object, owner string) Object {
	summary := ""
	for _, line := range strings.Split(fleet.Text(d["description"]), "\n") {
		if strings.TrimSpace(line) != "" {
			summary = strings.TrimSpace(line)
			break
		}
	}
	r := []rune(summary)
	if len(r) > 200 {
		summary = string(r[:200])
	}
	v := Object{"name": owner + "/" + fleet.Text(d["name"]), "displayName": d["name"], "description": summary, "autoMerge": d["autoMerge"], "autoDeploy": d["autoDeploy"], "testCommand": d["testCommand"]}
	if d["setupCommand"] != nil {
		v["setupCommand"] = d["setupCommand"]
	}
	return v
}
