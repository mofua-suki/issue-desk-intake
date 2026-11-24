package consultationai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"path/filepath"
	"sync"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/models"
)

type Record = consultations.Record
type Caller interface {
	Call(context.Context, ai.Request) (ai.Answer, error)
}
type Reference struct {
	Version string `json:"version"`
	Text    string `json:"text"`
}
type Image struct {
	ID, Ext, SHA256 string
	Bytes           []byte
}
type EnvironmentHost struct {
	Name   string
	Values map[string]any
}

type Options struct {
	Environment           func(context.Context, string) ([]EnvironmentHost, error)
	Config                func(context.Context) (map[string]any, error)
	Models                *models.Service
	Runner                Caller
	Gitea                 *gitea.Client
	Records               *consultations.Records
	RuntimeDir            string
	SchemaDir             string
	Now                   func() time.Time
	Reference             func(context.Context, Record, string) (Reference, error)
	ReferenceContinuation func(context.Context, Record, string) (Reference, error)
	// EagerOpenRequests は、重複照合の一覧をAIの最初の呼び出しの前に毎回渡す。
	EagerOpenRequests bool
	Images            func(context.Context, Record) ([]Image, error)
	Split             func(context.Context, Record, []string) (Record, error)
	ResolveNumbers    func(context.Context, Record) (any, error)
	ReadUsage         func(context.Context, string) (map[string]any, error)
	RecordUsage       func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error
	Wait              func(context.Context, time.Duration) error
}
type Service struct {
	rateWaits providerWaits
	options   Options
	blobs     blobCache
	trees     treeCache
	searches  searchCache
	mu        sync.Mutex
	locks     map[string]*sync.Mutex
	resolve   func(context.Context, map[string]any, map[string]any, string) (models.Settings, error)
}

func New(options Options) (*Service, error) {
	if options.Config == nil || options.Models == nil || options.Runner == nil || options.Gitea == nil || options.Records == nil || !filepath.IsAbs(options.RuntimeDir) || !filepath.IsAbs(options.SchemaDir) {
		return nil, errors.New("相談AIの設定・モデル・実行器・資料・保存先が必要です")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Wait == nil {
		options.Wait = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	resolve := func(ctx context.Context, c, d map[string]any, step string) (models.Settings, error) {
		var normalized map[string]any
		if json.Unmarshal(marshal(d), &normalized) != nil {
			return models.Settings{}, errors.New("モデルの対象を確認できません")
		}
		return options.Models.Resolve(ctx, c, normalized, step)
	}
	return &Service{options: options, locks: map[string]*sync.Mutex{}, resolve: resolve}, nil
}
func (s *Service) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[id] == nil {
		s.locks[id] = &sync.Mutex{}
	}
	return s.locks[id]
}
func numericValue(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
func marshal(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
func copyRecord(d Record) Record {
	var out Record
	decoder := json.NewDecoder(bytes.NewReader(marshal(d)))
	decoder.UseNumber()
	_ = decoder.Decode(&out)
	return out
}

// Every checkpoint retains the current run ownership; a replaced or withdrawn turn cannot write.
func (s *Service) save(ctx context.Context, d Record, fields Record) error {
	if d["_ephemeral"] == true {
		for key, value := range fields {
			d[key] = value
		}
		return nil
	}
	_, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || current["status"] != "thinking" || current["run"] != d["run"] {
			return nil, errors.New("相談の実行権を失いました")
		}
		for key, value := range fields {
			current[key], d[key] = value, value
		}
		return current, nil
	})
	return err
}
