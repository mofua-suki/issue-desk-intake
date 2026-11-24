package admincli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/startup"
)

func cliKey(ctx context.Context, config *startup.Config, action string, in io.Reader, out io.Writer) error {
	s, err := openAppStore(ctx, config)
	if err != nil {
		return err
	}
	defer s.Close()
	if action == "revoke" {
		raw, err := io.ReadAll(io.LimitReader(in, 128))
		key := strings.TrimSpace(string(raw))
		if err != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(key) {
			return errors.New("一時キーの標準入力が不正です")
		}
		if _, err = s.DB().ExecContext(ctx, "DELETE FROM cli_keys WHERE hash=?", hash(key)); err != nil {
			return errors.New("一時キーを取り消せません")
		}
		return nil
	}
	key, err := newToken("")
	if err != nil {
		return err
	}
	if err = s.AddCLIKey(ctx, hash(key), time.Now().Add(time.Minute)); err != nil {
		return errors.New("一時キーを保存できません")
	}
	_, err = fmt.Fprintln(out, key)
	return err
}
