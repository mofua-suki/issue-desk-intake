---
name: check-gitea-issues
description: LinuxのDockerで動くissue-desk-workerの監視を起動・停止・点検する。受付が配った許可済みの仕事だけを実装する。アプリの予約タスクには登録しない。
---

<!-- skill-standard: 1 -->

# Issueの監視とWorkerの運用

受付は`issue-desk-intake`、実装を動かす本体は`issue-desk-worker`。対象リポジトリのAGENTS.mdを先に読む。導入とチャット窓口は[チャットとLinux PCへの配布](../../docs/chat-operations.md)に従う。

- 状態を聞かれたら起動せず、[host-drain](../host-drain/SKILL.md) の一覧手順と対象PCの`docker compose -f <compose.yamlの絶対パス> ps`を読む。ログも同じcomposeから読む。
- 起動を依頼されているときだけ、対象PCのコンテナで`/usr/local/bin/worker check --config /state/config.local.json`を実行してからcomposeの`up -d`を使う。停止はcomposeの`stop`を使う。
- Workerは受付APIから割り当てられた仕事だけを実行する。周期は受付が公開するassignments.pollSecondsを使う。別の予約タスク・ランナー・同じ状態への二重起動を足さない。
- K2はworkerId・serverUrl・serverTokenFileの3項目。担当表やモデルなどをK2へ複製しない。受付の`GET /api/v1/client-config?host=<workerId>`が公開する設定を使う。
- 監視用Giteaトークンは`/state/private/worker-token`に置く。`token-place`は担当表のworkerAccountsの本人とGiteaの`/user`を照合する。値を表示・コピーしてAIへ渡さない。新しいトークンや権限を勝手に発行しない。
- 設定・CLI認証・自己検査・受付のheartbeatを別々に確かめる。`~/.issue-desk/desk doctor`は受付接続の診断であり、AI契約認証や実装能力の検査全体を代用しない。
- Workerの認証が原因なら、Workerリポジトリ（`mofua-suki/issue-desk-worker`）の資料を参照する。Codexの再ログインは `docs/codex-login.md`、Claudeの利用上限時に運用者がAPIキーを配置・更新する手順は `docs/api-key.md`。
- PCの引き取り停止・再開と再起動前後の安全確認は [host-drain](../host-drain/SKILL.md) に従う。必要な権限がない場合は権限を拡大しない。
- Issueへの追加指示・停止・再開・再試行は受付の画面またはissue-desk-issue-control Skillを使う。止まった原因を確かめずに再試行しない。Giteaへコマンドを直接投稿して受付を迂回しない。
- state、outbox、実装のsession/branch、認証volumeを削除して復旧しない。設定と原因を直して既存状態のまま再開する。
- WorkerのイメージはNASが登録した希望版へhostdが入れ替える。マージしたWorker自身が自己配置する前提を置かない。Server自身の配置はNASのServer更新手順で行い、Workerの更新と混ぜない。
