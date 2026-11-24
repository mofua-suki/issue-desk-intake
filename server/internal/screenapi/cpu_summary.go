package screenapi

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
)

type callFrame struct {
	FunctionName string `json:"functionName"`
	URL          string `json:"url"`
	LineNumber   *int   `json:"lineNumber"`
	ColumnNumber *int   `json:"columnNumber"`
	ScriptID     string `json:"scriptId"`
}
type profileNode struct {
	ID       int64      `json:"id"`
	Frame    *callFrame `json:"callFrame"`
	Children []int64    `json:"children"`
}
type cpuRow struct {
	FunctionName string   `json:"functionName"`
	File         string   `json:"file"`
	Line         *int     `json:"line"`
	Category     string   `json:"category"`
	SelfMS       *float64 `json:"selfMs"`
	TotalMS      *float64 `json:"totalMs"`
}

func displayName(value string) string {
	if value == "" {
		return "(anonymous)"
	}
	if !regexp.MustCompile(`^[\w.$ ()<>\[\]-]{1,120}$`).MatchString(value) || regexp.MustCompile(`(?i)token|password|secret|authorization|[A-Za-z0-9_-]{33,}`).MatchString(value) {
		return "[redacted]"
	}
	return value
}
func SummarizeCPU(raw []byte, root string) (Object, error) {
	var profile struct {
		Nodes   []profileNode `json:"nodes"`
		Samples []any         `json:"samples"`
		Deltas  []any         `json:"timeDeltas"`
	}
	invalid := func() (Object, error) { return nil, fail(422, "プロファイルの形式が不正です。") }
	if !validCPUProfileShape(raw) {
		return invalid()
	}
	if json.Unmarshal(raw, &profile) != nil || len(profile.Nodes) == 0 {
		return invalid()
	}
	tree, ok := newCPUProfileTree(profile.Nodes, root)
	if !ok {
		return invalid()
	}
	used, missing, observed := tree.accumulate(profile.Samples, profile.Deltas)
	values := []cpuRow{}
	for _, key := range tree.order {
		row := *tree.rows[key]
		if used == 0 {
			row.SelfMS = nil
			row.TotalMS = nil
		}
		values = append(values, row)
	}
	var measured any
	if used > 0 {
		measured = observed
	}
	return Object{"estimated": true, "basis": "samples/timeDeltas による標本からの推定値（ms）", "samples": used, "missingSamples": missing, "incomplete": used == 0 || missing > 0, "observedMs": measured, "self": rankCPURows(values, false), "total": rankCPURows(values, true)}, nil
}

// validCPUProfileShape はプロファイルのJSONが、整数の識別子を持つノードの配列と標本の配列を持つかを確かめる。
func validCPUProfileShape(raw []byte) bool {
	var input Object
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	rawNodes, ok := input["nodes"].([]any)
	if !ok || len(rawNodes) == 0 {
		return false
	}
	for _, v := range rawNodes {
		node, ok := v.(map[string]any)
		if !ok {
			return false
		}
		id, ok := node["id"].(float64)
		if !ok || id < 0 || id > 9007199254740991 || math.Trunc(id) != id {
			return false
		}
		if children, has := node["children"]; has {
			if _, ok := children.([]any); !ok {
				return false
			}
		}
	}
	for _, key := range []string{"samples", "timeDeltas"} {
		if value, has := input[key]; has {
			if _, ok := value.([]any); !ok {
				return false
			}
		}
	}
	return true
}

// cpuProfileTree はプロファイルのノードの親子関係と、同じ関数の位置をまとめた行。
type cpuProfileTree struct {
	nodes   map[int64]profileNode
	parents map[int64]int64
	keys    map[int64]string
	rows    map[string]*cpuRow
	order   []string
}

// newCPUProfileTree はノードを関数の位置ごとの行にまとめ、親子関係が重複・欠落・循環の無い木かを確かめる。
func newCPUProfileTree(profileNodes []profileNode, root string) (*cpuProfileTree, bool) {
	nodes := map[int64]profileNode{}
	parents := map[int64]int64{}
	keys := map[int64]string{}
	rows := map[string]*cpuRow{}
	order := []string{}
	for _, node := range profileNodes {
		if node.ID < 0 || node.ID > 9007199254740991 || node.Frame == nil {
			return nil, false
		}
		if _, has := nodes[node.ID]; has {
			return nil, false
		}
		nodes[node.ID] = node
		frame := node.Frame
		file, category := profileLocation(frame.URL, root)
		switch frame.FunctionName {
		case "(idle)":
			category = "idle"
		case "(garbage collector)":
			category = "gc"
		case "(root)":
			category = "root"
		}
		var line *int
		if frame.LineNumber != nil && *frame.LineNumber >= 0 {
			n := *frame.LineNumber + 1
			line = &n
		}
		key := signature([]any{frame.FunctionName, frame.URL, line, frame.ColumnNumber, frame.ScriptID})
		keys[node.ID] = key
		if rows[key] == nil {
			self, total := 0.0, 0.0
			rows[key] = &cpuRow{displayName(frame.FunctionName), file, line, category, &self, &total}
			order = append(order, key)
		}
	}
	for _, node := range profileNodes {
		for _, child := range node.Children {
			if _, has := nodes[child]; !has {
				return nil, false
			}
			if _, has := parents[child]; has {
				return nil, false
			}
			parents[child] = node.ID
		}
	}
	checked := map[int64]bool{}
	for id := range nodes {
		chain := map[int64]bool{}
		at := id
		for !checked[at] {
			if chain[at] {
				return nil, false
			}
			chain[at] = true
			parent, has := parents[at]
			if !has {
				break
			}
			at = parent
		}
		for n := range chain {
			checked[n] = true
		}
	}
	return &cpuProfileTree{nodes: nodes, parents: parents, keys: keys, rows: rows, order: order}, true
}

// accumulate は標本ごとの時間を、その関数の自己時間と祖先を含む合計時間へ足し、使った標本・欠けた標本の数と観測した時間を返す。
func (t *cpuProfileTree) accumulate(samples, deltas []any) (int, int, float64) {
	nodes, parents, keys, rows := t.nodes, t.parents, t.keys, t.rows
	missing, used := 0, 0
	observed := 0.0
	n := len(samples)
	if len(deltas) > n {
		n = len(deltas)
	}
	for i := 0; i < n; i++ {
		if i >= len(samples) || i >= len(deltas) {
			missing++
			continue
		}
		sample, sampleOK := samples[i].(float64)
		delta, deltaOK := deltas[i].(float64)
		id := int64(sample)
		if _, has := nodes[id]; !sampleOK || !deltaOK || math.Trunc(sample) != sample || !has || math.IsNaN(delta) || math.IsInf(delta, 0) || delta < 0 {
			missing++
			continue
		}
		amount := delta / 1000
		used++
		observed += amount
		*rows[keys[id]].SelfMS += amount
		seen := map[string]bool{}
		at := id
		for {
			key := keys[at]
			if !seen[key] {
				*rows[key].TotalMS += amount
				seen[key] = true
			}
			parent, has := parents[at]
			if !has {
				break
			}
			at = parent
		}
	}
	return used, missing, observed
}

// rankCPURows は自己時間（total なら合計時間）の長い順に上位20行を返す。計れなかった行は後ろに置く。
func rankCPURows(values []cpuRow, total bool) []cpuRow {
	out := append([]cpuRow{}, values...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].SelfMS, out[j].SelfMS
		if total {
			a, b = out[i].TotalMS, out[j].TotalMS
		}
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a > *b
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
