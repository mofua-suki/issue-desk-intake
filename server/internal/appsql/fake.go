package appsql

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
)

// Step describes one ordered operation. Errors and values are supplied by tests.
type Step struct {
	Operation, SQL string
	Args           []any
	Options        *sql.TxOptions
	Context        context.Context
	Result         Result
	Rows           Rows
	Err            error
}
type Fake struct {
	Steps    []Step
	at       int
	mismatch bool
}

func (f *Fake) take(op string, ctx context.Context, q string, args []any, opts *sql.TxOptions) (Step, error) {
	if f.at >= len(f.Steps) {
		f.mismatch = true
		return Step{}, errors.New("unexpected fake operation")
	}
	s := f.Steps[f.at]
	f.at++
	if s.Operation != op || s.SQL != q || !reflect.DeepEqual(s.Args, args) || !reflect.DeepEqual(s.Options, opts) || (s.Context != nil && s.Context != ctx) {
		f.mismatch = true
		return Step{}, errors.New("fake operation mismatch")
	}
	return s, s.Err
}
func (f *Fake) Verify() error {
	if f.mismatch || f.at != len(f.Steps) {
		return errors.New("fake expectations not met")
	}
	return nil
}
func (f *Fake) Begin(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	_, e := f.take("begin", ctx, "", nil, opts)
	if e != nil {
		return nil, e
	}
	return f, nil
}
func (f *Fake) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	s, e := f.take("exec", ctx, q, args, nil)
	return s.Result, e
}
func (f *Fake) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	s, e := f.take("query", ctx, q, args, nil)
	return s.Rows, e
}
func (f *Fake) Commit() error   { _, e := f.take("commit", nil, "", nil, nil); return e }
func (f *Fake) Rollback() error { _, e := f.take("rollback", nil, "", nil, nil); return e }

type FakeResult struct {
	ID, Affected       int64
	IDErr, AffectedErr error
}

func (r FakeResult) LastInsertId() (int64, error) { return r.ID, r.IDErr }
func (r FakeResult) RowsAffected() (int64, error) { return r.Affected, r.AffectedErr }

// FakeRows assigns test values to compatible pointer destinations.
type FakeRows struct {
	Values [][]any
	Index  int
	Error  error
	Closed bool
}

func (r *FakeRows) Next() bool {
	if r.Index >= len(r.Values) {
		return false
	}
	r.Index++
	return true
}
func (r *FakeRows) Scan(dest ...any) error {
	if r.Index == 0 || r.Index > len(r.Values) || len(dest) != len(r.Values[r.Index-1]) {
		return errors.New("fake scan mismatch")
	}
	for i, v := range r.Values[r.Index-1] {
		p := reflect.ValueOf(dest[i])
		if p.Kind() != reflect.Pointer || p.IsNil() {
			return errors.New("fake scan destination invalid")
		}
		if v == nil {
			p.Elem().SetZero()
			continue
		}
		x := reflect.ValueOf(v)
		if !x.Type().AssignableTo(p.Elem().Type()) {
			return errors.New("fake scan type mismatch")
		}
		p.Elem().Set(x)
	}
	return nil
}
func (r *FakeRows) Err() error   { return r.Error }
func (r *FakeRows) Close() error { r.Closed = true; return nil }
