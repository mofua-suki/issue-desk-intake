---
name: project-intake
description: 開発受付アプリの「新しいプロジェクト」で依頼者と対話し、まだ無いリポジトリのプロジェクト定義（名前・説明・技術・配置先・固定テスト・自動化・最初の分割案）を確定する。
---

<!-- skill-standard: 1 -->

# 新しいプロジェクトの相談

相談で確かめる項目、定義を確定する条件、引継ぎ時の進め方は、受付が読み込む [新しいプロジェクトの相談手順](../../server/internal/projects/prompt.md) に従う。応答の形式は [schema.json](../../server/internal/projects/schema.json) を参照する。

利用者がチャットで確定済みの定義に同意した場合は、[新しいプロジェクトの作成同意](../issue-desk-issue-control/refs/api-operations.md) の手順で受付へ送る。
