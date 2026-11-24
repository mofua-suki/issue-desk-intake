// fleet・割り当ての読み込みと、ファイルの更新印による読み込み結果のキャッシュ・複製。

package fleet

import (
	"errors"
	"os"
	"reflect"
	"time"
)

type fileStamp struct {
	path     string
	size     int64
	modified time.Time
}

type loadCache struct {
	fleet, assignments fileStamp
	document           Document
	value              *Assignments
}

func stamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{path, info.Size(), info.ModTime()}, nil
}

// Copy the parsed values without JSON serialization, preserving their concrete
// types and nil values as well as the private normalization state.
func cloneLoadValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		if v.Kind() == reflect.Interface {
			c := reflect.New(v.Type()).Elem()
			c.Set(cloneLoadValue(v.Elem()))
			return c
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(cloneLoadValue(v.Elem()))
		return c
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c.SetMapIndex(iter.Key(), cloneLoadValue(iter.Value()))
		}
		return c
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(cloneLoadValue(v.Index(i)))
		}
		return c
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if c.Field(i).CanSet() && v.Type().Field(i).IsExported() {
				c.Field(i).Set(cloneLoadValue(v.Field(i)))
			}
		}
		return c
	default:
		return v
	}
}

func cloneLoad(document Document, assignments *Assignments) (Document, *Assignments) {
	return cloneLoadValue(reflect.ValueOf(document)).Interface().(Document), cloneLoadValue(reflect.ValueOf(assignments)).Interface().(*Assignments)
}

func (s *Service) Load() (Document, *Assignments, error) {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	fleetStamp, fleetErr := stamp(s.FleetPath)
	assignmentStamp, assignmentErr := stamp(s.AssignmentsPath)
	if fleetErr == nil && assignmentErr == nil && s.loadCache != nil && s.loadCache.fleet == fleetStamp && s.loadCache.assignments == assignmentStamp {
		document, assignments := cloneLoad(s.loadCache.document, s.loadCache.value)
		return document, assignments, nil
	}
	s.loadCache = nil
	document, err := readFleetDocument(s.FleetPath)
	if err != nil {
		return document, nil, err
	}
	var raw map[string]any
	if err := ReadJSON(s.AssignmentsPath, &raw); err != nil {
		return document, nil, err
	}
	assignments, err := NormalizeAssignments(raw)
	if err == nil {
		err = validateNativeTests(document, assignments)
	}
	if err == nil && fleetErr == nil && assignmentErr == nil {
		fleetAfter, e1 := stamp(s.FleetPath)
		assignmentAfter, e2 := stamp(s.AssignmentsPath)
		if e1 == nil && e2 == nil && fleetStamp == fleetAfter && assignmentStamp == assignmentAfter {
			cachedDocument, cachedAssignments := cloneLoad(document, assignments)
			s.loadCache = &loadCache{fleetStamp, assignmentStamp, cachedDocument, cachedAssignments}
		}
	}
	return document, assignments, err
}

// readFleetDocument reads the fleet file and validates its defaults and per-PC settings.
func readFleetDocument(path string) (Document, error) {
	var document Document
	if err := ReadJSON(path, &document); err != nil {
		return document, err
	}
	if document.Version != 1 || document.Hosts == nil || document.Defaults == nil {
		return document, errors.New("fleetの形式が不正です")
	}
	if err := checkConfig(document.Defaults); err != nil {
		return document, err
	}
	for host, config := range document.Hosts {
		if !ValidHost(host) {
			return document, errors.New("fleetのPC名が不正です")
		}
		if err := checkConfig(config); err != nil {
			return document, err
		}
	}
	return document, nil
}
