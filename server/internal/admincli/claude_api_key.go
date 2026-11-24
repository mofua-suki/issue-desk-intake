package admincli

import (
	"io"
	"strings"

	"issue-desk-intake/server/internal/claudeapikey"
	"issue-desk-intake/server/internal/startup"
)

func claudeAPIKey(config *startup.Config, action string, in io.Reader) error {
	store := claudeapikey.New(config.TokenFile)
	if action == "clear" {
		return store.Clear()
	}
	data, err := io.ReadAll(io.LimitReader(in, 4097))
	if err != nil || len(data) > 4096 {
		return claudeapikey.ErrInvalidKey
	}
	return store.Set(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"))
}
