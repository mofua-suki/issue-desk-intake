package appsql

import "encoding/json"

// JSONColumn is the Go type a JSON column is scanned into. The choice stays
// with the caller because a NULL column fails Scan into string but scans into
// a nil []byte, which then fails to decode.
type JSONColumn interface{ ~string | ~[]byte }

// JSONRowsOptions selects how JSONObjects and JSONObjectsByID treat each row.
type JSONRowsOptions struct {
	// SkipInvalid skips and counts rows whose JSON cannot be decoded instead
	// of failing the whole read. Scan errors always fail.
	SkipInvalid bool
	// Observe, when set, receives the byte length of every scanned row (the
	// id and JSON columns together) before it is decoded.
	Observe func(size int)
}

// JSONObjects reads rows of one JSON column and decodes each into a map,
// keeping the query order. It always closes rows.
//
// A scan error, or a decode error without SkipInvalid, returns nil, 0 and that
// error. Otherwise it returns the decoded rows (never nil), the number of
// skipped rows and the error of rows.Err (or else rows.Close); with that last
// error the rows read so far are still returned.
func JSONObjects[C JSONColumn](rows Rows, opts JSONRowsOptions) ([]map[string]any, int, error) {
	out := []map[string]any{}
	skipped, err := scanJSONRows[C](rows, opts, false, func(_ string, value map[string]any) {
		out = append(out, value)
	})
	if err != nil && skipped < 0 {
		return nil, 0, err
	}
	return out, skipped, err
}

// JSONObjectsByID reads rows of (id, JSON) columns and decodes each JSON into
// a map keyed by id; a later row with the same id replaces the earlier one.
// Errors, the skipped count and closing follow JSONObjects.
func JSONObjectsByID[C JSONColumn](rows Rows, opts JSONRowsOptions) (map[string]map[string]any, int, error) {
	out := map[string]map[string]any{}
	skipped, err := scanJSONRows[C](rows, opts, true, func(id string, value map[string]any) {
		out[id] = value
	})
	if err != nil && skipped < 0 {
		return nil, 0, err
	}
	return out, skipped, err
}

// scanJSONRows returns a negative count when a row failed to scan or decode,
// so the callers can drop the partial result.
func scanJSONRows[C JSONColumn](rows Rows, opts JSONRowsOptions, withID bool, add func(string, map[string]any)) (skipped int, err error) {
	defer func() {
		if e := rows.Close(); err == nil {
			err = e
		}
	}()
	for rows.Next() {
		var id string
		var raw C
		if withID {
			err = rows.Scan(&id, &raw)
		} else {
			err = rows.Scan(&raw)
		}
		if err != nil {
			return -1, err
		}
		if opts.Observe != nil {
			opts.Observe(len(id) + len(raw))
		}
		// A JSON null decodes to a nil map, as json.Unmarshal sets the map
		// to nil even when it starts empty.
		var value map[string]any
		if e := json.Unmarshal([]byte(raw), &value); e != nil {
			if !opts.SkipInvalid {
				return -1, e
			}
			skipped++
			continue
		}
		add(id, value)
	}
	return skipped, rows.Err()
}
