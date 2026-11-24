package consultationai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"path"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

const maxFile, maxProject, maxSource = 160000, 600000, 4000000
const maxCandidates = 20

type ReadRequest struct {
	File    string `json:"file"`
	Mode    string `json:"mode"`
	Start   int    `json:"start,omitempty"`
	End     int    `json:"end,omitempty"`
	Keyword string `json:"keyword,omitempty"`
	Context int    `json:"context"`
}
type Document struct {
	Request           ReadRequest `json:"request"`
	Project           string      `json:"project"`
	Path              string      `json:"path"`
	SHA256            string      `json:"sha256"`
	Version           string      `json:"version"`
	Text              string      `json:"text,omitempty"`
	Next              int         `json:"next,omitempty"`
	Reason            string      `json:"reason,omitempty"`
	Candidates        []string    `json:"candidates,omitempty"`
	CandidatesOmitted int         `json:"candidatesOmitted,omitempty"`
	Size              int         `json:"size"`
}
type treeEntry struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	Size int    `json:"size"`
}
type projectState struct {
	Name     string     `json:"name"`
	Branch   string     `json:"branch"`
	Revision string     `json:"revision"`
	Reads    []Document `json:"reads"`
	entries  map[string]treeEntry
	bytes    int
}
type contextState struct {
	InitialDocuments           map[string]bool          `json:"initialDocuments"`
	InitialDocumentsCollecting bool                     `json:"initialDocumentsCollecting,omitempty"`
	Projects                   map[string]*projectState `json:"projects"`
	Rounds                     int                      `json:"rounds"`
	Pending                    []ReadRequest            `json:"pending"`
	Remaining                  int                      `json:"pendingRemaining"`
	Notice                     string                   `json:"notice,omitempty"`
	// Delivered は、この相談で受付AIへ本文を配信した資料の鍵（documentKey）。会話の破棄（圧縮）やプロンプトからの本文の除外のあとも残る。
	Delivered map[string]bool `json:"delivered,omitempty"`
}

var sha40 = regexp.MustCompile(`^[a-f0-9]{40}$`)
var projectName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var publicDotFile = regexp.MustCompile(`^(?:\.env\.(?:example|sample|template|dist)|\.gitattributes|\.gitignore|\.dockerignore|\.editorconfig|\.prettierignore|\.prettierrc(?:\.[\w-]+)?)$`)
var secretDirectory = regexp.MustCompile(`(?i)(?:^|/)(?:private|data|images|downloads|profiles|node_modules|secrets?)(?:/|$)`)

// rootModelsDirectory は、リポジトリの直下の models/（機械学習の重みなどの置き場）だけを読ませない。
// server/internal/models/ のような、ソースコードのパッケージ名としての models は読める（読めないと、その変更の確認が終わらない）。
// 重みのファイルそのものは、どの階層でも binaryName が拒否する。
var rootModelsDirectory = regexp.MustCompile(`(?i)^models(?:/|$)`)
var secretName = regexp.MustCompile(`(?i)(?:^|[./_-])(?:local|secret|token|credentials|auth)(?:[./_-]|$)`)
var binaryName = regexp.MustCompile(`(?i)\.(?:png|jpe?g|gif|webp|ico|bmp|pdf|zip|jar|gz|7z|tar|exe|dll|so|wasm|db|sqlite3?|bin|onnx|pt|pth|ckpt|gguf|npz|h5|safetensors)$`)
var privateKey = regexp.MustCompile(`(?i)^(?:id_rsa|id_ed25519)$|\.(?:pem|key|p12|pfx|dpapi)$`)

func contextAllowed(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || path.IsAbs(name) || secretDirectory.MatchString(name) || rootModelsDirectory.MatchString(name) || secretName.MatchString(name) || binaryName.MatchString(name) || privateKey.MatchString(path.Base(name)) {
		return false
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") && (i != len(parts)-1 || !publicDotFile.MatchString(part)) {
			return false
		}
		// runtime は通常実行時データの置き場。internal/runtime 直下の Go ソースだけ例外とする。
		if strings.EqualFold(part, "runtime") && (i == 0 || parts[i-1] != "internal" || i != len(parts)-2 || path.Ext(parts[i+1]) != ".go") {
			return false
		}
	}
	return !strings.Contains(path.Base(name), ".env") || publicDotFile.MatchString(path.Base(name))
}
func request(value any) (ReadRequest, error) {
	r := ReadRequest{Mode: "full", Context: 5}
	if name, ok := value.(string); ok {
		r.File = name
	} else {
		v := fleet.Object(value)
		if v == nil {
			return r, errors.New("資料の読み取り要求が不正です")
		}
		r.File, r.Mode, r.Keyword = fleet.Text(v["file"]), fleet.Text(v["mode"]), fleet.Text(v["keyword"])
		for key, target := range map[string]*int{"start": &r.Start, "end": &r.End, "context": &r.Context} {
			if n, exists := v[key]; exists && n != nil {
				switch n.(type) {
				case json.Number, float64, int, int64:
				default:
					return r, errors.New("資料の行範囲は整数で指定してください")
				}
				x := fleet.Number(n)
				if x != float64(int(x)) || x < 0 || x > 9007199254740991 {
					return r, errors.New("資料の行範囲が不正です")
				}
				*target = int(x)
			}
		}
	}
	if r.File == "" || r.End != 0 && r.Start > r.End || r.Mode == "lines" && (r.Start < 1 || r.End < r.Start) || r.Context < 0 || r.Context > 50 {
		return r, errors.New("資料の読み取り範囲が不正です")
	}
	switch r.Mode {
	case "full", "outline", "lines":
	case "keyword":
		if r.Keyword == "" || strings.ContainsAny(r.Keyword, "\r\n") || contract.UTF16Length(r.Keyword) > 200 {
			return r, errors.New("検索語は改行のない200文字以内の文字列です")
		}
	default:
		return r, errors.New("資料の読み取り方式が不正です")
	}
	file := r.File
	if before, after, ok := strings.Cut(file, ":"); ok && projectName.MatchString(before) {
		file = after
	}
	if file == "repo-search" && r.Mode != "keyword" {
		return r, errors.New("repo-searchはkeywordだけで要求してください")
	}
	return r, nil
}
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func requestKey(r ReadRequest) string { return string(marshal(r)) }

func ContextFileAllowed(name string) bool { return contextAllowed(name) }
