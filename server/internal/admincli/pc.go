package admincli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"issue-desk-intake/server/internal/startup"
	"issue-desk-intake/server/internal/store"
)

func validatePCIssueName(name string) error {
	if name == "" {
		return errors.New("PC名を入力してください")
	}
	if len(name) > 31 {
		return errors.New("PC名は1〜31文字にしてください")
	}
	if !(name[0] >= 'a' && name[0] <= 'z' || name[0] >= '0' && name[0] <= '9') {
		return errors.New("PC名の先頭は小文字英字または数字にしてください")
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return errors.New("PC名に使える文字は小文字英字、数字、ハイフンです")
		}
	}
	return nil
}

func pc(ctx context.Context, config *startup.Config, action, name string, out, notice io.Writer) error {
	if action == "issue" {
		if err := validatePCIssueName(name); err != nil {
			return err
		}
	}
	s, err := openAppStore(ctx, config)
	if err != nil {
		return err
	}
	defer s.Close()
	switch action {
	case "issue":
		return issuePCToken(ctx, s, name, out, notice)
	case "revoke":
		return revokePCToken(ctx, s, name, notice)
	case "list":
		return listPCTokens(ctx, s, out, notice)
	}
	return errUsage
}

// issuePCToken issues a new token for the PC and prints it once.
func issuePCToken(ctx context.Context, s *store.SQLite, name string, out, notice io.Writer) error {
	token, err := newToken("idk_")
	if err != nil {
		return err
	}
	id, err := uuid()
	if err != nil {
		return err
	}
	added, err := s.AddPCToken(ctx, store.PCTokenRecord{ID: id, Name: name, Hash: hash(token), IssuedAt: time.Now()})
	if err != nil {
		if errors.Is(err, store.ErrInvalidPCName) {
			return errors.New("PC名の形式が不正です")
		}
		return errors.New("PCトークンを保存できません")
	}
	if !added {
		return errors.New("有効なPCトークンがあるため発行しません。必要なら先に失効してください")
	}
	fmt.Fprintln(notice, "PCトークンを発行しました。標準出力の値は今回だけ表示します。保護されたGit管理外のファイルへ保存してください。")
	_, err = fmt.Fprintln(out, token)
	return err
}

// revokePCToken revokes the PC's active token.
func revokePCToken(ctx context.Context, s *store.SQLite, name string, notice io.Writer) error {
	found, err := s.RevokePCToken(ctx, name)
	if err != nil {
		return errors.New("PCトークンを失効できません")
	}
	if !found {
		return errors.New("対象の有効なPCトークンはありません")
	}
	fmt.Fprintln(notice, "PCトークンを失効させました。次の要求から反映します。")
	return nil
}

// listPCTokens prints the issued tokens by name with their last use.
func listPCTokens(ctx context.Context, s *store.SQLite, out, notice io.Writer) error {
	rows, err := s.DB().QueryContext(ctx, bindSQL("SELECT name,issued_at,last_used_at FROM api_tokens ORDER BY name"))
	if err != nil {
		return errors.New("PCトークンの一覧を読めません")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, issued string
		var used sql.NullString
		if rows.Scan(&name, &issued, &used) != nil {
			return errors.New("PCトークンの一覧を読めません")
		}
		last := used.String
		if !used.Valid {
			last = "未使用"
		}
		if _, err = fmt.Fprintf(out, "%s\t発行: %s\t最終使用: %s\n", name, issued, last); err != nil {
			return err
		}
		count++
	}
	if rows.Err() != nil {
		return errors.New("PCトークンの一覧を読めません")
	}
	if count == 0 {
		fmt.Fprintln(notice, "発行済みのPCトークンはありません。")
	}
	return nil
}
