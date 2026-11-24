# SSH申請の期限切れ確定

共通の`service.ssh`は、要求内で一度取得したnowと現在のhost-ssh方針のTTLを使い、期限切れを独立したトランザクションで確定する。通常はUPDATE 1回で、判定用のapproved全件SELECTを行わない。状態は更新時にもapprovedに限定する。

## 判定と保存

期限判定は`queryMaps` → `iso` → `moment`の時刻変換と同じ意味を維持する。UTCでミリ秒へ切り捨てたdecided_atがnow−TTL以下ならexpiredにする。now自身はミリ秒へ切り捨てない。

NULL、infinity、-infinityは期限判定の対象外とする。紀元前1年はGoの年0000として解析できるため対象に含める。紀元前2年以前、10000年以上の年は文字列化後に解析できないため除外する。不正文字列はtimestamptzへの格納段階で拒否される。DBドライバーや時刻変換を変更するときは、格納可能な特殊時刻の読取成否も確認する。

期限切れの更新は後続操作とは独立してコミットするため、後続操作が拒否されても確定した期限切れは残る。期限切れUPDATEに失敗した場合はロールバックし、後続操作へ進まない。保存形式・マイグレーションは変更しない。期限切れ前処理を変更するときも、APIの認可・応答形式・TTL・発券条件を維持する。

## 検証の実行

READMEと同じGo 1.27、Node.js 24、pnpm 11.26.0と破棄可能な隔離PostgreSQLを使う。専用DSNなしの直接go testはSKIPになるため、次のラッパーでDBを自動作成する。PostgreSQL配布物はREADME記載の固定版とSHA-512で検証される。

```sh
node scripts/with-test-postgres.ts -- sh -c 'cd server && go test -count=1 -run "TestSSHExpiry|TestSSHTicketScreenApprovalSignerAndResultOwner" -v ./internal/adminapi'
CGO_ENABLED=1 node scripts/with-test-postgres.ts -- sh -c 'cd server && go test -race -count=1 -run "TestSSHExpiry|TestSSHTicketScreenApprovalSignerAndResultOwner" -v ./internal/adminapi'
sh scripts/verify.sh
```

SSH試験は通常の`_test.go`なので、固定検証の`go test -count=1 ./...`にも含まれる。race実行にはCGOとCコンパイラが必要になる。
