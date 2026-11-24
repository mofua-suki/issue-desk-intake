package startup

import (
	"encoding/json"
	"math"
)

func positiveNumber(value any) (int64, bool) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 9007199254740991 || n != math.Trunc(n) {
		return 0, false
	}
	return int64(n), true
}
