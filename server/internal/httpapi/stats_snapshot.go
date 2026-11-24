// 統計の計算中に使う相談一覧の読み取り結果を1回に揃え、呼び出しごとに複製して渡す。

package httpapi

import (
	"context"
	"reflect"
	"sync"
)

// statsDraftSnapshot belongs to one calculation; every consumer receives its own copy.
func statsDraftSnapshot(read func(context.Context) ([]map[string]any, error)) func(context.Context) ([]map[string]any, error) {
	if read == nil {
		return nil
	}
	var once sync.Once
	var drafts []map[string]any
	var err error
	return func(ctx context.Context) ([]map[string]any, error) {
		once.Do(func() {
			var source []map[string]any
			source, err = read(ctx)
			if err == nil {
				drafts = cloneStatsDraftValue(reflect.ValueOf(source)).Interface().([]map[string]any)
			}
		})
		if err != nil {
			return nil, err
		}
		return cloneStatsDraftValue(reflect.ValueOf(drafts)).Interface().([]map[string]any), nil
	}
}

// Records contain JSON-like maps and slices, possibly with named container types.
func cloneStatsDraftValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneStatsDraftValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneStatsDraftValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneStatsDraftValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneStatsDraftValue(v.Index(i)))
		}
		return out
	default:
		return v
	}
}
