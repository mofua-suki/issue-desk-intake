// 統計の応答から詳細を除いて要約にする処理と、詳細・取得不能を見分ける判定。

package httpapi

import (
	"reflect"
)

func applyStatsDetails(value any, details bool) {
	if !details {
		summarizeStats(reflect.ValueOf(value), "")
	}
}

// summarizeStats removes only documented detail paths, preserving unrelated aggregates.
func summarizeStats(v reflect.Value, path string) {
	if !v.IsValid() {
		return
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if !v.IsNil() {
			summarizeStats(v.Elem(), path)
		}
		return
	}
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		for _, k := range v.MapKeys() {
			next := k.String()
			if path != "" {
				next = path + "." + next
			}
			if statsDetailPath(next) {
				v.SetMapIndex(k, reflect.Value{})
				continue
			}
			summarizeStats(v.MapIndex(k), next)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			summarizeStats(v.Index(i), path+"[]")
		}
	}
}

func statsDetailPath(path string) bool {
	for _, period := range []string{"before", "after"} {
		for _, name := range []string{"drafts", "sessions", "repairs", "contextReadPaths", "contextReadPathsOmitted", "issues", "omittedIssues"} {
			if path == "usage."+period+"."+name {
				return true
			}
		}
		if path == "implementation."+period+".issues" || path == "inputBudget."+period+".records" {
			return true
		}
	}
	switch path {
	case "tiers.rows", "tiers.omitted", "followups.rows", "followups.omitted", "breakdown.issues", "implementation.issues", "breakdown.followups.groups[].issues":
		return true
	}
	return false
}

// statsUnavailable recognizes explicit partial-read indicators returned by statistics.
func statsUnavailable(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		return statsUnavailable(v.Elem())
	}
	switch v.Kind() {
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if it.Key().Kind() == reflect.String && it.Key().String() == "unavailable" {
				x := it.Value()
				for x.IsValid() && (x.Kind() == reflect.Interface || x.Kind() == reflect.Pointer) {
					if x.IsNil() {
						break
					}
					x = x.Elem()
				}
				if x.IsValid() && (x.Kind() == reflect.Slice || x.Kind() == reflect.Array) && x.Len() > 0 {
					return true
				}
			}
			if statsUnavailable(it.Value()) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if statsUnavailable(v.Index(i)) {
				return true
			}
		}
	}
	return false
}
