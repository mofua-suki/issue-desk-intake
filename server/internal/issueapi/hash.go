package issueapi

// 操作IDと内容の要約値（ハッシュ）を作る。パッケージ全体の操作の識別に使う。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func operationID(value string) string {
	sum := sha256.Sum256([]byte(value))
	b := sum[:16]
	b[6] = (b[6] & 15) | 80
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func digest(value any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(value)))
	return hex.EncodeToString(sum[:])
}
