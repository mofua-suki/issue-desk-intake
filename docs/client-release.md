# Workerの望む版の登録

各PCのhostdは、受付が返す「望む版」にだけ追従する。イメージの発行と登録が要る。Workerのtrunkへのマージは、NASの更新（10分周期の `deploy/nas/update.sh`）が自動で発行・登録する。

## 自動の発行と登録

`deploy/nas/update-worker-release.sh` がServer更新の最後に動く。Workerのtrunkの先頭をミラー（`runtime/worker-release/repo.git`）に取り、前回登録した版（`runtime/worker-release/registered`）と違えば、`-python` のイメージを `127.0.0.1:5000` へ発行して、`deploy/nas/worker-release.json` の `hosts` に宣言した全PCへ、PCごとのレジストリ名で登録する。

- 変更の基点（`baseRevision`）は前回この段が登録した版にする。受付の共通（`*`）の行の版とは限らず、PC専用の行の現在の版と一致しないと、種類が `align`（すぐ引き取りを止めて更新）になる。
- 前回の登録から60分未満は、`align` の変更を含まない版の発行と登録を待ち、`[update:worker-release-waiting]` と次に登録できる時刻を日本標準時（+0900）でログに出す。60分後の更新で前回の版からの変更をまとめて登録する。`align` の変更は待たない。初回や、前回の時刻・版・差分を判断できない場合も待たずに登録を試みる。時刻は `registered` の更新時刻を使う。
- 同じ版の失敗は1時間おく（`runtime/worker-release/failed`）。ビルド・pushのログは `build.log`・`push.log`。
- ツール定義が使用中のツールイメージの基点から変わった場合、Workerの対象版からツールイメージを構築して発行を続ける。構築ログは `runtime/worker-release/tools-build.log`。使用するイメージは `runtime/worker-release/tools-image` に保存し、ファイルが無効またはイメージが存在しない場合は `.env` の `ISSUE_DESK_TOOLS_IMAGE` に戻る。ServerのビルドではCLIツールのベースに `.env` の値を使い、CodexはServerのDockerfileで別途固定する。
- 古いツールイメージは自動削除しない。手動で整理する場合は `docker image ls issue-desk-tools` で確認し、稼働中の `runtime/worker-release/tools-image` と `.env` の `ISSUE_DESK_TOOLS_IMAGE` と受付Dockerfileの `CODEX_TOOLS_IMAGE` に記載されたイメージを残して、不要なものを `docker image rm <イメージ名>` で削除する。
- 失敗しても更新の他の段は止めない。ログの `[update:worker-release-failed]` で見分け、成功は `[update:worker-release-success]`。
- PCを足す・外すときは `worker-release.json` の `hosts` を直す。NASの `.env` の `REGISTRY` と一致させる。

## 登録

NASの受付内で `intake admin ... client-release register`（JSONは標準入力）を使う。項目は `host`（省略は共通 `*`）・`flavor`（`""` か `-python`）・`registry`・`tag`・`digest`・`revision`、任意で `baseRevision`・`changedFiles`。`tag` は `revision` の先頭12桁（`-python` は `-client-python` を付ける）。登録できた直後に警告が出ることがある（下記）。

## 決まり（登録で間違えやすい点）

- **PC専用の行が共通の行に優先する**（`server/internal/hostd/service.go` の `Desired`、`ORDER BY (host=$1) DESC`）。共通（`*`）の行だけ登録しても、PC専用の行がある PC の望む版は変わらず、エラーも出ない。共通の行を登録したときに、別の版で残っているPC専用の行があれば警告する。
- **レジストリ名はPCごとに違う**。hostdは、起動中のcomposeのimageが「登録のregistry/issue-desk-client<flavor>:tag」と一致しないと更新を戻す（`digest_mismatch`）。NASは `.env` の `REGISTRY=127.0.0.1:5000`、他のPCは `ugreen-nas.example.ts.net:5443`。PC専用の行のレジストリ名が、そのPCの前回の登録と違うときは警告する。
- **失敗した版は消えない**。`host_image_results` の `failed` は上書きされず、同じタグは再試行できない。登録を間違えた版は、新しいコミット（新しいタグ）で出し直す。

## hostdの失敗理由

更新後に確認するregistry・digestが合わないときは、待たずに戻して `digest_mismatch` と報告する。`ready_timeout` は、起動後にreadyを待って時間切れになったときだけ。
