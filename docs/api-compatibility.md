# API契約

APIの正本は `api/openapi/`。`api/openapi.json` は同じ定義の結合版で、Workerが取得する版付き成果物として使う。`api/types.ts` はブラウザ用のwire型であり、Serverの内部実装や資格情報へ依存しない。

## 契約版と成果物

契約ヘッダーは `x-issue-desk-contract`。ヘッダー名・固定の契約版・エラーの回復案内は [api/contract.json](../api/contract.json) を参照する。GoとTypeScriptは同じ版を照合する。

API変更時は分割YAMLと結合版JSONをともに更新し、`node --test test/openapi-read-contract.test.ts` で内容とキー順の一致を確かめる。無関係な並び替えが必要な場合は先に両形式の一致を確認し、整形だけのコミットを分ける。Go側の版・ヘッダー・回復案内・ブラウザ型・必須経路は `server/internal/contract/contract_test.go` で検査する。

分割仕様はファイル名順に結合し、同じ経路の後の定義で値を置き換え、最初の経路位置を保つ。`50-issues-drafts.yaml` の相談操作への参照は、後の `52-draft-operations.yaml` の定義を元の経路順に置くために必要である。移動先の経路を変える場合は参照先と結合順も合わせる。

成果物のSHA-256一覧は `node scripts/api-manifest.ts --print` で表示できる。引数なしの検査は契約版の形式と `api/manifest.json` がないことを確認する。

Workerは版を固定したAPI成果物だけを取り込み、Serverの実装をimportしない。OpenAPIに経路が存在することは、その経路がServerで実装済みという意味ではない。未実装の操作を成功応答や空一覧で代用しない。

## 認証と権限

PCトークン (`idk_`)、hostd (`idh_`)、画面セッション、ループバックからの一時キーを別の認証区分として扱う。Authorizationを提示した要求は、拒否後にCookieや一時キーへ戻さない。トークンは平文で保存せず、SHA256を永続化ストアへ照合する。保存先を読めない場合は認証を通さず503を返す。

画面の変更操作には同一OriginとCSRFを必要とする。画面専用の承認は、運用代行を許されたPCトークンや一時キーでも実行できない。ClientはServerが返す `actions` を表示に使い、Serverは同じ状態判定で操作を検証する。

一時キーはループバック接続でのみ原子的に1回消費する。Forwarded・X-Forwarded系・Tailscale系ヘッダーがある要求にはループバックの特権を与えない。内部Hostでは `/api/v1/` のトークン要求だけを受け付ける。

`internal/auth` は資格情報の照合と認証区分・送り手の境界を提供する。永続化は必須のStore interfaceで接続する。各ルートのプロジェクト許可・所有者・状態・同意の検証を置き換えるものではない。

## 再送と排他

Serverは応答に契約ヘッダーを付ける。APIの要求に契約ヘッダーがあり受付の版と違えば、業務処理の前に409 `CONTRACT_MISMATCH` を返す（`server/internal/startup/run.go`）。ヘッダーのない要求は版を理由に断らない。版の更新の判断は利用側が `CONTRACT_MISMATCH` の回復案内に従って行う。

エラーの `retryable` は真偽値ではなく `same-request` / `new-request`。認証失敗・通信断・副作用が不明な場合は同じ送信IDと内容を保持する。Serverは現在 `retryable: new-request`・`safeToChange: true` を返さず、送信IDを変えてよいと示すエラー応答は無い。

`lease_lost` / `turn_lost` は機械可読な識別子として維持する。回復案内が一般の拒否と同じ場合も識別子を上書きしない。ジョブのhost・epoch・有効期限は行ロック内で照合する。ジョブ終了後のevents/notesには最後のhost/epochだけを認める。マージの番のepochとジョブleaseのepochを混同しない。

## JSONの境界

jobs APIのHTTP本文上限は128KiBの受信bytes。JSONオブジェクトだけを受け付け、配列・null・二重のJSON値・不明な入力項目を拒否する。重複キーは `JSON.parse` と同じく最終値を採用する。

相談文字数や一部のJSONサイズはJavaScriptと同じUTF-16コード単位で数える。日本語のBMP文字は1、絵文字は2。HTTPのbytesとは区別し、`contract.UTF16Length` を文字数計算に使う。JSONの数値は `json.Number` としてデコードし、各ルートで安全整数・正数・型を検証する。

OpenAPIに空schema `{}` がある場合も、入力を無制限に許可しない。各ルートの認証・副作用・再送・拒否コードを契約のfixtureと照合する。

## 保存

PostgreSQLに相談・認証・共通番号（アプリ用スキーマ）とジョブ・配布・運用状態を保存する。ServerだけがDBへ接続し、WorkerとClientはAPIを呼ぶ。DBの更新はトランザクションを使い、共通番号と送信IDの一意性を保持する。

## Issue検索の応答時間

`GET /api/issues/search` は既定100件ずつ返し、`hasMore` が真なら `page` を増やして続ける。`countOnly=1` は件数だけを返す。検索行の `dispatchRecord` と `executionSettings` は詳細から読む。詳細では `shownBody` が `body` と同じなら `shownBody` を省く。

検索は全Issueから条件判定用の行を作り、選択ページだけ表示用のジョブ投影を行う。件数だけの指定では表示用のジョブ投影を行わない。status・host・priority・shownTitle の判定にはジョブ、下書き、番号の照合が必要で、全体の番号割当と終了記録も行う。表示に必要な失敗し得る読取は従来の行順で実施する。

詳細の番号表示は要求内でリポジトリごとに解決済み・未登録の結果を再利用する。DBエラーは未解決として表示し、次の参照で再照会する。一覧は一括取得済みのスナップショットを使う。依存配列の他リポジトリ照会や別のジョブ・待機表示はそれぞれの経路で処理する。

番号解決のDB照会数は詳細全体のDB照会数ではない。詳細の応答時間にはGitea・通信・他の処理も含まれるため、応答時間の差を番号解決だけの寄与時間とは断定できない。
