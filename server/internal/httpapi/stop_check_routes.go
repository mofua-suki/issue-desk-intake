package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/statistics"
)

const stopCheckBodyLimit = 1024 * 1024
const stopCheckCountLimit = 1000

func (s *WorkerRoutes) stopCheckRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true, CLI: true})
	if !ok {
		return
	}
	if err := workerMethod(r, http.MethodPost); err != nil {
		workerFailure(w, err)
		return
	}
	bad := func() {
		workerFailure(w, contract.NewProblem(400, "発動記録の形式または大きさが不正です。", ""))
	}
	records, valid := parseStopCheckRecords(r, principal)
	if !valid {
		bad()
		return
	}
	tx, err := s.options.DB.BeginTx(r.Context(), nil)
	if err != nil {
		workerFailure(w, err)
		return
	}
	defer tx.Rollback()
	if err = statistics.WriteStopCheckRecords(r.Context(), tx, records); err != nil {
		if errors.Is(err, statistics.ErrInvalidStopCheck) {
			bad()
		} else {
			workerFailure(w, err)
		}
		return
	}
	if err = tx.Commit(); err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, map[string]any{"ok": true}, http.StatusOK)
}

// parseStopCheckRecords は検査の発動記録の本文を読み、項目の重複や欠けが無い記録だけを送信元のPCの記録として返す。形が不正なら false を返す。
func parseStopCheckRecords(r *http.Request, principal auth.Principal) ([]statistics.StopCheckRecord, bool) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, stopCheckBodyLimit+1))
	if err != nil || len(body) > stopCheckBodyLimit {
		return nil, false
	}
	var entries []json.RawMessage
	if json.Unmarshal(body, &entries) != nil || entries == nil || len(entries) > stopCheckCountLimit {
		return nil, false
	}
	host := principal.Name
	if principal.Kind == auth.CLI {
		host = "nas"
	}
	records := make([]statistics.StopCheckRecord, 0, len(entries))
	for _, raw := range entries {
		entry, err := stopCheckFields(raw)
		if err != nil || len(entry) != 5 {
			return nil, false
		}
		fields := make([]string, 5)
		for i, key := range []string{"id", "checkId", "result", "at", "project"} {
			value, present := entry[key]
			if !present || string(value) == "null" || json.Unmarshal(value, &fields[i]) != nil {
				return nil, false
			}
		}
		records = append(records, statistics.StopCheckRecord{ID: fields[0], CheckID: fields[1], Result: fields[2], OccurredAt: fields[3], Project: fields[4], Host: host})
	}
	return records, true
}

// stopCheckFields rejects duplicate keys before decoding values into a map.
func stopCheckFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate key")
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	_, err = dec.Token()
	return fields, err
}
