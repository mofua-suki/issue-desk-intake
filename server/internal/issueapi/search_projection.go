package issueapi

// Search retains fallible reads in list order and delays batched display-only
// formatting until the selected page is known.
type searchProjectionKey struct{}
type searchProjections struct {
	rows []func() error
}

func newSearchProjections() *searchProjections { return &searchProjections{} }
func (p *searchProjections) add(row Object, f func() error) {
	row["_searchProjection"] = len(p.rows)
	p.rows = append(p.rows, f)
}
func (p *searchProjections) apply(row Object) error {
	value, ok := row["_searchProjection"].(int)
	delete(row, "_searchProjection")
	if ok {
		return p.rows[value]()
	}
	return nil
}
