package appsql

import "errors"

// BinaryCollation returns the collation name that orders text by its bytes, to
// be written after the COLLATE keyword.
func (d Dialect) BinaryCollation() (string, error) {
	if !d.valid() {
		return "", errors.New("unknown SQL dialect")
	}
	return `"C"`, nil
}

func (Active) BinaryCollation() (string, error) { return CurrentDialect().BinaryCollation() }
