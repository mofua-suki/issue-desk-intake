// Package images validates and stores attachments independently of their transport.
package images

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io"
	"regexp"

	"issue-desk-intake/server/internal/contract"
)

const MaxBytes = 2 * 1024 * 1024
const MaxPixels = 12000000
const MaxDimension = 6000
const PerMessage = 4
const PerConversation = 12

type Inspection struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	MIME   string `json:"mime"`
	Ext    string `json:"ext"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

func invalid() *contract.Problem {
	return contract.NewProblem(400, "PNG/JPEG画像を確認してください（1枚2MiB、1200万画素、縦横6000pxまで）。", "")
}
func dimensions(width, height int) bool {
	return width > 0 && height > 0 && width <= MaxDimension && height <= MaxDimension && width*height <= MaxPixels
}
func Inspect(data []byte) (Inspection, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return Inspection{}, invalid()
	}
	var result Inspection
	var e error
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}):
		result, e = inspectPNG(data)
	case len(data) >= 2 && data[0] == 255 && data[1] == 216:
		result, e = inspectJPEG(data)
	default:
		e = invalid()
	}
	if e != nil {
		return Inspection{}, e
	}
	sum := sha256.Sum256(data)
	result.Size = len(data)
	result.SHA256 = hex.EncodeToString(sum[:])
	return result, nil
}
func inspectPNG(data []byte) (Inspection, error) {
	out := Inspection{MIME: "image/png", Ext: "png"}
	position, depth, color := 8, 0, 0
	ended, header := false, false
	compressed := []byte{}
	for position+12 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[position : position+4]))
		if size > len(data)-position-12 {
			return out, invalid()
		}
		end := position + 12 + size
		kind := string(data[position+4 : position+8])
		if crc32.ChecksumIEEE(data[position+4:end-4]) != binary.BigEndian.Uint32(data[end-4:end]) {
			return out, invalid()
		}
		if !header && kind != "IHDR" {
			return out, invalid()
		}
		switch kind {
		case "IHDR":
			if header || size != 13 {
				return out, invalid()
			}
			out.Width = int(binary.BigEndian.Uint32(data[position+8 : position+12]))
			out.Height = int(binary.BigEndian.Uint32(data[position+12 : position+16]))
			if !dimensions(out.Width, out.Height) {
				return out, invalid()
			}
			depth, color = int(data[position+16]), int(data[position+17])
			valid := false
			for _, candidate := range map[int][]int{0: {1, 2, 4, 8, 16}, 2: {8, 16}, 3: {1, 2, 4, 8}, 4: {8, 16}, 6: {8, 16}}[color] {
				if depth == candidate {
					valid = true
				}
			}
			if !valid || data[position+18] != 0 || data[position+19] != 0 || data[position+20] != 0 {
				return out, invalid()
			}
			header = true
		case "IDAT":
			compressed = append(compressed, data[position+8:end-4]...)
		case "IEND":
			if size != 0 || end != len(data) {
				return out, invalid()
			}
			ended = true
		default:
			if kind == "acTL" || (kind != "PLTE" && kind != "tRNS" && kind[0] >= 'A' && kind[0] <= 'Z') {
				return out, invalid()
			}
		}
		position = end
		if ended {
			break
		}
	}
	if !ended || len(compressed) == 0 {
		return out, invalid()
	}
	channels := map[int]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[color]
	rowSize := (out.Width*channels*depth+7)/8 + 1
	reader, e := zlib.NewReader(bytes.NewReader(compressed))
	if e != nil {
		return out, invalid()
	}
	defer reader.Close()
	bounded := io.LimitReader(reader, int64(rowSize*out.Height)+1)
	row := make([]byte, rowSize)
	for y := 0; y < out.Height; y++ {
		if _, e = io.ReadFull(bounded, row); e != nil || row[0] > 4 {
			return out, invalid()
		}
	}
	var extra [1]byte
	n, e := bounded.Read(extra[:])
	if n != 0 || e != io.EOF {
		return out, invalid()
	}
	return out, nil
}
func inspectJPEG(data []byte) (Inspection, error) {
	out := Inspection{MIME: "image/jpeg", Ext: "jpg"}
	position := 2
	header, scan := false, false
	for position < len(data) {
		if data[position] != 255 {
			return out, invalid()
		}
		position++
		for position < len(data) && data[position] == 255 {
			position++
		}
		if position >= len(data) {
			return out, invalid()
		}
		marker := data[position]
		position++
		if marker == 217 {
			if !scan || !header || position != len(data) {
				return out, invalid()
			}
			return out, nil
		}
		if marker == 0 || marker == 216 || (marker >= 208 && marker <= 215) || position+2 > len(data) {
			return out, invalid()
		}
		size := int(binary.BigEndian.Uint16(data[position : position+2]))
		if size < 2 || position+size > len(data) {
			return out, invalid()
		}
		if marker == 192 || marker == 193 || marker == 194 {
			if size < 8 || header || data[position+2] != 8 {
				return out, invalid()
			}
			out.Width = int(binary.BigEndian.Uint16(data[position+5 : position+7]))
			out.Height = int(binary.BigEndian.Uint16(data[position+3 : position+5]))
			if !dimensions(out.Width, out.Height) {
				return out, invalid()
			}
			header = true
		} else if marker >= 192 && marker <= 207 && marker != 196 && marker != 200 && marker != 204 {
			return out, invalid()
		}
		position += size
		if marker == 218 {
			if !header {
				return out, invalid()
			}
			scan = true
			start := position
			for position < len(data) {
				if data[position] != 255 {
					position++
					continue
				}
				if position+1 < len(data) && (data[position+1] == 0 || (data[position+1] >= 208 && data[position+1] <= 215)) {
					position += 2
					continue
				}
				break
			}
			if position == start {
				return out, invalid()
			}
		}
	}
	return out, invalid()
}

var base64Image = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

func Decode(value string) ([]byte, Inspection, error) {
	if len(value) > (MaxBytes+2)/3*4 || len(value)%4 != 0 || !base64Image.MatchString(value) {
		return nil, Inspection{}, invalid()
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(value)
	if e != nil || base64.StdEncoding.EncodeToString(raw) != value {
		return nil, Inspection{}, invalid()
	}
	info, e := Inspect(raw)
	return raw, info, e
}
