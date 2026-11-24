package filingcheck

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Record = consultations.Record
type Options struct {
	Records             *consultations.Records
	Gitea               *gitea.Client
	Jobs                *jobs.Store
	AppDB, Postgres     *sql.DB
	Config              func(context.Context) (map[string]any, error)
	Project             func(string) error
	Own                 func(map[string]any) bool
	DeskNumber          func(string, int64) any
	ResolvePreceding    func(context.Context, Record) ([]Record, error)
	Answer              func(context.Context, map[string]any) (map[string]any, error)
	Material            func(context.Context, Record) (Record, error)
	Revision, SchemaDir string
	Now                 func() time.Time
}
type evidenceReader interface {
	Get(context.Context, string, string) (Record, bool, error)
}

type Service struct {
	evidenceReads evidenceReader
	options       Options
	locks         sync.Map
	schema        string
}

func New(options Options) (*Service, error) {
	if options.Records == nil || options.Config == nil || options.Answer == nil || options.Project == nil || options.Material == nil && (options.Gitea == nil || options.Jobs == nil || options.Own == nil) {
		return nil, errors.New("起票前の比較資料・設定・保存先・判定が必要です")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	schema, err := os.ReadFile(filepath.Join(options.SchemaDir, "filing-check.schema.json"))
	if err != nil {
		return nil, err
	}
	return &Service{options: options, evidenceReads: options.Records, schema: string(schema)}, nil
}
func obj(v any) Record { return fleet.Object(v) }
func raw(v any) []byte { b, _ := json.Marshal(v); return b }
func clone(v Record) Record {
	var out Record
	d := json.NewDecoder(bytes.NewReader(raw(v)))
	d.UseNumber()
	_ = d.Decode(&out)
	return out
}
func fail(message string) error { return contract.NewProblem(409, message, "") }
func stamp(t time.Time) string  { return t.UTC().Format(time.RFC3339Nano) }
func (s *Service) lock(id string) *sync.Mutex {
	value, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func (s *Service) update(ctx context.Context, d Record, fields Record) error {
	_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || fleet.WholeNumber(current["revision"]) != fleet.WholeNumber(d["revision"]) || current["status"] != d["status"] || current["run"] != d["run"] {
			return nil, fail("比較中に相談が更新されました。最新の相談で続けてください。")
		}
		next := clone(current)
		for k, v := range fields {
			next[k] = v
			d[k] = v
		}
		return next, nil
	})
	return err
}
