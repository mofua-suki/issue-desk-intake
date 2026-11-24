// pprof の CPU プロファイル（protobuf）を最小限に読み解き、関数ごとの標本数へまとめる。

package adminapi

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strconv"
)

// protobuf fields needed by the pprof profile format; unknown fields are skipped.
type protoField struct {
	number int
	value  uint64
	data   []byte
}

func fields(b []byte) ([]protoField, error) {
	var out []protoField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, errors.New("bad protobuf tag")
		}
		b = b[n:]
		f := protoField{number: int(tag >> 3)}
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			f.value = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[8:]
		case 2:
			v, n := binary.Uvarint(b)
			if n <= 0 || v > uint64(len(b)-n) {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[n:]
			f.data = b[:int(v)]
			b = b[int(v):]
		case 5:
			if len(b) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[4:]
		default:
			return nil, errors.New("bad wire type")
		}
		out = append(out, f)
	}
	return out, nil
}

func packed(b []byte) ([]uint64, error) {
	var out []uint64
	for len(b) > 0 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad packed value")
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}

type cpuSample struct {
	locations []uint64
	values    []uint64
}

func parseCPUProfile(data []byte) ([]profileEntry, []profileEntry, int64, error) {
	p, e := decodeCPUProfile(data)
	if e != nil {
		return nil, nil, 0, e
	}
	entries, total, e := p.functionTimes()
	if e != nil {
		return nil, nil, 0, e
	}
	self, cumulative := rankProfileEntries(entries)
	return self, cumulative, total, nil
}

type profileFunction struct{ name, file uint64 }
type profileLine struct{ function, line uint64 }

// cpuProfile holds the parts of a decoded pprof profile that the summary uses.
type cpuProfile struct {
	strings []string
	samples []cpuSample
	funcs   map[uint64]profileFunction
	locs    map[uint64][]profileLine
	types   []uint64
}

// decodeCPUProfile decodes the gzip-compressed pprof protobuf.
func decodeCPUProfile(data []byte) (*cpuProfile, error) {
	z, e := gzip.NewReader(bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(z)
	z.Close()
	if e != nil {
		return nil, e
	}
	top, e := fields(raw)
	if e != nil {
		return nil, e
	}
	p := &cpuProfile{funcs: map[uint64]profileFunction{}, locs: map[uint64][]profileLine{}}
	for _, f := range top {
		switch f.number {
		case 6:
			p.strings = append(p.strings, string(f.data))
		case 1:
			a, err := fields(f.data)
			if err != nil {
				return nil, err
			}
			var typ uint64
			for _, v := range a {
				if v.number == 1 {
					typ = v.value
				}
			}
			p.types = append(p.types, typ)
		case 2:
			a, err := fields(f.data)
			if err != nil {
				return nil, err
			}
			var s cpuSample
			for _, v := range a {
				if v.number == 1 || v.number == 2 {
					var nums []uint64
					if v.data != nil {
						nums, err = packed(v.data)
					} else {
						nums = []uint64{v.value}
					}
					if err != nil {
						return nil, err
					}
					if v.number == 1 {
						s.locations = append(s.locations, nums...)
					} else {
						s.values = append(s.values, nums...)
					}
				}
			}
			p.samples = append(p.samples, s)
		case 4:
			a, err := fields(f.data)
			if err != nil {
				return nil, err
			}
			var id uint64
			var fs []profileLine
			for _, v := range a {
				if v.number == 1 {
					id = v.value
				}
				if v.number == 4 {
					line, err := fields(v.data)
					if err != nil {
						return nil, err
					}
					var info profileLine
					for _, x := range line {
						if x.number == 1 {
							info.function = x.value
						}
						if x.number == 2 {
							info.line = x.value
						}
					}
					fs = append(fs, info)
				}
			}
			p.locs[id] = fs
		case 5:
			a, err := fields(f.data)
			if err != nil {
				return nil, err
			}
			var id, name, file uint64
			for _, v := range a {
				if v.number == 1 {
					id = v.value
				}
				if v.number == 2 {
					name = v.value
				}
				if v.number == 4 {
					file = v.value
				}
			}
			p.funcs[id] = profileFunction{name: name, file: file}
		}
	}
	return p, nil
}

// functionTimes attributes each sample's CPU time to its leaf function (self)
// and once to every function on its stack (cumulative).
func (p *cpuProfile) functionTimes() (map[string]*profileEntry, int64, error) {
	cpu, count := -1, -1
	for i, x := range p.types {
		if x < uint64(len(p.strings)) {
			switch p.strings[x] {
			case "cpu":
				cpu = i
			case "samples":
				count = i
			}
		}
	}
	if cpu < 0 || count < 0 {
		return nil, 0, errors.New("missing sample types")
	}
	entries := map[string]*profileEntry{}
	var total int64
	for _, s := range p.samples {
		if cpu >= len(s.values) || count >= len(s.values) {
			return nil, 0, errors.New("missing sample values")
		}
		total += int64(s.values[count])
		seen := map[string]bool{}
		first := true
		for _, loc := range s.locations {
			for _, frame := range p.locs[loc] {
				function, ok := p.funcs[frame.function]
				idx := function.name
				if !ok || idx == 0 || idx >= uint64(len(p.strings)) {
					continue
				}
				name := p.strings[idx]
				if entries[name] == nil {
					entries[name] = &profileEntry{Function: name}
				}
				if entries[name].File == "" && function.file > 0 && function.file < uint64(len(p.strings)) && frame.line > 0 {
					entries[name].File = p.strings[function.file] + ":" + strconv.FormatUint(frame.line, 10)
				}
				if first {
					entries[name].Self += int64(s.values[cpu])
					first = false
				}
				if !seen[name] {
					entries[name].Cumulative += int64(s.values[cpu])
					seen[name] = true
				}
			}
		}
	}
	return entries, total, nil
}

// rankProfileEntries orders the functions by self and by cumulative time.
func rankProfileEntries(entries map[string]*profileEntry) ([]profileEntry, []profileEntry) {
	all := make([]profileEntry, 0, len(entries))
	for _, v := range entries {
		all = append(all, *v)
	}
	self := append([]profileEntry{}, all...)
	cumulative := append([]profileEntry{}, all...)
	sort.Slice(self, func(i, j int) bool {
		if self[i].Self == self[j].Self {
			return self[i].Function < self[j].Function
		}
		return self[i].Self > self[j].Self
	})
	sort.Slice(cumulative, func(i, j int) bool {
		if cumulative[i].Cumulative == cumulative[j].Cumulative {
			return cumulative[i].Function < cumulative[j].Function
		}
		return cumulative[i].Cumulative > cumulative[j].Cumulative
	})
	return self, cumulative
}
