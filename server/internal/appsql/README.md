# アプリケーション保存用 SQL 部品

受付の保存先は PostgreSQL だけで、方言も `Postgres` だけを持つ。呼出側がスキーマとトランザクションの責務を持つ。

`Bind` は SQL 中の `?` を PostgreSQL の `$1` からの番号に変える。単一・二重引用、行コメント、ドル引用、入れ子のブロックコメントを保持する。`E'...'` のバックスラッシュエスケープ文字列も保持する（文字列内の `?` は置換しない）。閉じない引用やコメントはエラー。番号付きの `?N`（`?1` 等）と、既存 `$n` と `?` の混在もエラー。PostgreSQL の jsonb の `?`・`?|`・`?&` 演算子は `Bind` では検出できず、`data ? 'k'` は `data $1 'k'` に書き換わる。呼出側はこれらの演算子を使わない（`jsonb_exists` 等の関数を使う）。識別子の引用規則や SQL の妥当性を検証するパーサーではない。

`InsertIgnore` は単純な識別子と `VALUES ...` または `SELECT ...` を受け、引数は別に渡す。1文のみ、RETURNING や既存の conflict 節は対象外。SELECT の内容（WHERE の有無を含む）は検査も付与もせず、渡された文字列に前置・後置の句を付けるだけである。`ON CONFLICT DO NOTHING` は一意性等の競合だけを無視し、NOT NULL・CHECK・外部キー違反はエラーとなる。

`RowOrder` の `source_rowid` は行の追加順を表す。すべての対象表に存在するわけではなく、呼出側が対象スキーマを確認する。`Identity` は新規表用の列定義であり、移行済みテーブルやカウンターを変更・初期化しない。

例: `q, err := Postgres.InsertIgnore("items", []string{"id"}, "VALUES (?)")` の結果を `tx.Exec(ctx, q, id)` へ渡す。`SQLStore{DB: db}` と `Within(ctx, store, &sql.TxOptions{ReadOnly: true}, fn)` は呼出側の context とオプションを database/sql に渡す。`Within` はコールバック失敗や panic 時に rollback し、成功時は commit エラーを返す。rollback エラーは返さない。`Fake` は順序付き期待値で同じ呼出経路を検査する。SQL エンジンではなく、引数比較エラーに値を含めない。呼出側は SQL やドライバーエラーをログへ出す際も秘密値を扱わないこと。

`JSONObjects[C](rows, opts)` は JSON の列1つを持つ行を、`JSONObjectsByID[C](rows, opts)` は `(id, json)` の行を `map[string]any` に組み立てる。`C` は列を受ける型（`string` か `[]byte`）で、NULL の列は `string` では Scan の失敗、`[]byte` では復号の失敗になる。既定では復号できない行で全体を失敗にし、`SkipInvalid` でその行を飛ばして件数を返す。Scan の失敗はどちらでも全体を失敗にする（結果は nil、件数は 0）。`rows.Err`・`rows.Close` の失敗では読めた分を添えて返す。JSON の `null` は nil の map になる。`Observe` は各行の id と JSON の長さの合計を復号の前に受け取る。どちらも必ず `rows.Close` を呼ぶ。行ごとに別の列を読む・途中で抜ける・独自の文言で失敗する箇所は、この関数を使わず手で書く。
