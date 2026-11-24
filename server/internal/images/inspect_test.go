package images

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	canvas.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if e := png.Encode(&buffer, canvas); e != nil {
		t.Fatal(e)
	}
	return buffer.Bytes()
}
func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if e := jpeg.Encode(&buffer, image.NewGray(image.Rect(0, 0, 2, 2)), nil); e != nil {
		t.Fatal(e)
	}
	return buffer.Bytes()
}
func chunk(kind string, data []byte) []byte {
	result := make([]byte, len(data)+12)
	binary.BigEndian.PutUint32(result, uint32(len(data)))
	copy(result[4:8], kind)
	copy(result[8:], data)
	binary.BigEndian.PutUint32(result[len(result)-4:], crc32.ChecksumIEEE(result[4:len(result)-4]))
	return result
}
func syntheticPNG(width, height int, raw []byte) []byte {
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header, uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(height))
	header[8] = 8
	header[9] = 6
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	writer.Write(raw)
	writer.Close()
	result := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	result = append(result, chunk("IHDR", header)...)
	result = append(result, chunk("IDAT", compressed.Bytes())...)
	return append(result, chunk("IEND", nil)...)
}
func TestInspectChecksEntirePNGAndJPEG(t *testing.T) {
	for _, data := range [][]byte{pngBytes(t), jpegBytes(t)} {
		info, e := Inspect(data)
		if e != nil || info.Width != 2 || info.Height != 2 || len(info.SHA256) != 64 {
			t.Fatalf("検査: %+v %v", info, e)
		}
		decoded, other, e := Decode(base64.StdEncoding.EncodeToString(data))
		if e != nil || !bytes.Equal(decoded, data) || other.SHA256 != info.SHA256 {
			t.Fatal("base64の往復が異なります")
		}
	}
	valid := pngBytes(t)
	crcBad := append([]byte{}, valid...)
	crcBad[29] ^= 1
	trailing := append(append([]byte{}, valid...), 0)
	jpeg := jpegBytes(t)
	for name, data := range map[string][]byte{"CRC破損": crcBad, "PNG末尾追加": trailing, "IEND欠落": valid[:len(valid)-12], "zlib出力不足": syntheticPNG(1, 1, []byte{0, 0, 0}), "zlib出力超過": syntheticPNG(1, 1, make([]byte, 100000)), "行フィルター不正": syntheticPNG(1, 1, []byte{5, 0, 0, 0, 0}), "寸法超過": syntheticPNG(6001, 1, []byte{0}), "画素超過": syntheticPNG(6000, 6000, []byte{0}), "JPEG終端欠落": jpeg[:len(jpeg)-2], "JPEG末尾追加": append(append([]byte{}, jpeg...), 0), "SVG": []byte(`<svg/>`)} {
		t.Run(name, func(t *testing.T) {
			if _, e := Inspect(data); e == nil {
				t.Fatal("未検証の画像を許しました")
			}
		})
	}
	encoded := base64.StdEncoding.EncodeToString(valid)
	for _, value := range []string{encoded + "\n", encoded[:len(encoded)-1], " " + encoded, stringsOfLength((MaxBytes+2)/3*4 + 4)} {
		if _, _, e := Decode(value); e == nil {
			t.Fatal("不正なbase64を許しました")
		}
	}
}
func stringsOfLength(size int) string { return string(bytes.Repeat([]byte{'A'}, size)) }
