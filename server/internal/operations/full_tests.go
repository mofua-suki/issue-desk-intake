package operations

import "context"

// FullTests は本線確認の失敗の一覧を返す。本線確認を登録する処理は今のGoに無く、残っているのは旧リポジトリの
// 記録（state/full-test-run.json と full_test_intake 表）だけなので、それらを読まず常に空の一覧を返す。
func (s *Service) FullTests(context.Context) ([]any, error) {
	return []any{}, nil
}
