package filingcheck

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// reduceNonOverlapping は、各判定の入力から範囲の重ならない候補の詳細を省く。
// 確認対象の変更範囲と重ならない候補は、題名と状態だけにする（重複は題名で、競合はパスの重なりで判断できる）。
// 対象の変更範囲が未確定（known=false・パス無し）のときは、重なりを判断できないので何も省かない。
// 省いた件数は omittedNonOverlapping に数え、省いた候補は nonOverlapping=true で題名だけを残す。
func reduceNonOverlapping(material Record) Record {
	out := clone(material)
	scope := obj(obj(out["target"])["changeScope"])
	targetPaths := scopePaths(scope)
	if scope["known"] == false || len(targetPaths) == 0 {
		return out
	}
	omitted := int64(0)
	for _, key := range []string{"requests", "jobs", "consultations"} {
		items := append([]any{}, fleet.Elements(out[key])...)
		for index, item := range items {
			row := obj(item)
			if row == nil || candidateOverlaps(row, targetPaths) {
				continue
			}
			short := Record{"nonOverlapping": true, "summaryOnly": true}
			for _, field := range []string{"id", "number", "issue", "title", "status", "state", "acceptsInstructions", "instructionBlocks"} {
				if value, exists := row[field]; exists {
					short[field] = value
				}
			}
			if job := obj(row["job"]); job != nil {
				shortJob := Record{}
				for _, field := range []string{"status", "dependsOn"} {
					if value, exists := job[field]; exists {
						shortJob[field] = value
					}
				}
				short["job"] = shortJob
			}
			items[index] = short
			omitted++
		}
		if out[key] != nil {
			out[key] = items
		}
	}
	out["omittedNonOverlapping"] = omitted
	return out
}

// scopePaths は変更範囲の宣言から、ファイルまたはディレクトリのパスを取り出す。
func scopePaths(scope Record) []string {
	var paths []string
	if scope == nil {
		return paths
	}
	for _, item := range fleet.Elements(scope["paths"]) {
		if p := strings.TrimSpace(fleet.Text(item)); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// candidateOverlaps は候補のパス（paths・scope・job.scope）が、対象のパスと同じか、一方が他方のディレクトリの下にあるかを返す。
// パスを1つも持たない候補は、重なりを判断できないので重なるものとして残す。
func candidateOverlaps(row Record, targetPaths []string) bool {
	var candidate []string
	collect := func(list any) {
		for _, item := range fleet.Elements(list) {
			if p := strings.TrimSpace(fleet.Text(item)); p != "" {
				candidate = append(candidate, p)
			}
		}
	}
	collect(row["paths"])
	collect(row["scope"])
	if scope := obj(row["changeScope"]); scope != nil {
		if scope["known"] == false {
			return true
		}
		collect(scope["paths"])
	}
	if job := obj(row["job"]); job != nil {
		collect(job["scope"])
	}
	if len(candidate) == 0 {
		return true
	}
	for _, c := range candidate {
		for _, t := range targetPaths {
			if samePathOrNested(c, t) {
				return true
			}
		}
	}
	return false
}
func samePathOrNested(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) || strings.HasSuffix(b, "/") && strings.HasPrefix(a, b)
}
