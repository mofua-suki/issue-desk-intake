package appsql

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestFakeTransactions(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "secret")
	failure := errors.New("failure")
	for _, tc := range []struct {
		name             string
		fail, commitFail bool
	}{{"success", false, false}, {"rollback", true, false}, {"commit failure", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &FakeRows{Values: [][]any{{int64(7)}}}
			opts := &sql.TxOptions{ReadOnly: true}
			steps := []Step{{Operation: "begin", Context: ctx, Options: opts}, {Operation: "query", Context: ctx, SQL: "select ?", Args: []any{"secret"}, Rows: rows}}
			if !tc.fail {
				steps = append(steps, Step{Operation: "commit", Err: func() error {
					if tc.commitFail {
						return failure
					}
					return nil
				}()})
			}
			steps = append(steps, Step{Operation: "rollback"})
			f := &Fake{Steps: steps}
			err := Within(ctx, f, opts, func(tx Tx) error {
				r, e := tx.Query(ctx, "select ?", "secret")
				if e != nil {
					return e
				}
				defer r.Close()
				if !r.Next() {
					return errors.New("missing row")
				}
				var n int64
				if e = r.Scan(&n); e != nil || n != 7 {
					return errors.New("bad row")
				}
				if tc.fail {
					return failure
				}
				return r.Err()
			})
			if (tc.fail || tc.commitFail) != errors.Is(err, failure) || !rows.Closed || f.Verify() != nil {
				t.Fatal("transaction expectation failed")
			}
		})
	}
	f := &Fake{Steps: []Step{{Operation: "exec", SQL: "x", Args: []any{"sensitive"}}}}
	_, err := f.Exec(ctx, "x", "different")
	if err == nil || f.Verify() == nil || err.Error() != "fake operation mismatch" {
		t.Fatal("mismatch was not safely detected")
	}
}

func TestFakePanicRollback(t *testing.T) {
	ctx := context.Background()
	f := &Fake{Steps: []Step{{Operation: "begin"}, {Operation: "rollback"}}}
	marker := &struct{}{}
	func() {
		defer func() {
			if recover() != marker {
				t.Error("panic not preserved")
			}
		}()
		_ = Within(ctx, f, nil, func(Tx) error { panic(marker) })
	}()
	if f.Verify() != nil {
		t.Fatal("rollback not called")
	}
}

func TestFakeCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &Fake{Steps: []Step{{Operation: "begin", Context: ctx, Err: context.Canceled}}}
	err := Within(ctx, f, nil, func(Tx) error { t.Error("callback called"); return nil })
	if !errors.Is(err, context.Canceled) || f.Verify() != nil {
		t.Fatal("begin cancellation not propagated")
	}
	f = &Fake{Steps: []Step{{Operation: "begin", Context: ctx}, {Operation: "exec", Context: ctx, SQL: "write", Err: context.Canceled}, {Operation: "rollback"}}}
	err = Within(ctx, f, nil, func(tx Tx) error { _, err := tx.Exec(ctx, "write"); return err })
	if !errors.Is(err, context.Canceled) || f.Verify() != nil {
		t.Fatal("cancellation did not roll back")
	}
}
