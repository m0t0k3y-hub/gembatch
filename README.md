# gembatch

Gemini API 用のコマンドラインツールです。**Batch API**（非同期・通常の **50% の料金**・目標処理時間24時間）と、**通常の同期API**（`run`、今すぐ結果が欲しいとき）の両方を、同じ入力形式・同じ出力形式で使えます。

- 標準ライブラリのみ・依存ゼロ・**単一バイナリ**（Windows / macOS / Linux、各約6MB）
- AIエージェントから呼べる設計：全コマンド `--json`、明確な終了コード、`gembatch usage --json`
- `skill/gembatch/SKILL.md` にエージェント用スキルを同梱

## 動作確認

`run`（同期API）と、`create` → `status` → `results`（バッチ）の一連の流れを、実際のGemini APIで確認済みです（画像3件）。ドキュメント上で揺れている点（状態名 `JOB_STATE_*` / `BATCH_STATE_*`、結果ファイルの場所 `response.responsesFile` / `dest.fileName`）は、どちらも読めるようにしてあります。想定外の挙動があれば `gembatch status <id> --raw` で生のレスポンスを確認してください。

## インストール

[リリースページ](https://github.com/m0t0k3y-hub/gembatch/releases)からOS用のバイナリをダウンロードして PATH に置くだけです。ソースからは `go install github.com/m0t0k3y-hub/gembatch@latest` または `go build -o gembatch .`（Go 1.24+）。

```bash
export GEMINI_API_KEY=...   # Google AI Studio で発行
gembatch version
```

## 使い方

### 通常のAPIで今すぐ実行（`run`）

バッチと同じ入力（`--dir` / `--prompts` / `--input`）をそのまま、同期APIで並列実行します（`--concurrency`、既定4）。429/5xxは自動で再試行します。結果の形式はバッチの `results` と同じです。

```bash
gembatch run --dir scans/ --prompt '台帳のこのページを文字起こしして' --limit 3          # まず3件だけ試す
gembatch run --prompts prompts.txt --out results.jsonl --concurrency 8
gembatch run --dir scans/ --prompt '...' --out-dir out/                                   # key.txt を1件ずつ
```

少数なら `run`、数百件以上で急がないなら `create`（バッチ、半額）と使い分けます。一部が失敗すると終了コード3になります。Ctrl-C すると、完了済みの分だけ出力して終了します（終了コード130）。

### バッチ（`create`）

```bash
# 1. 画像フォルダを丸ごと投入（1ファイル=1リクエスト、キーはファイル名）
gembatch create --dir scans/ --glob '*.png' --prompt '台帳のこのページを文字起こししてJSONで返して' --name ledger

# まずは --dry-run で中身を確認（APIキー不要）
gembatch create --dir scans/ --prompt '...' --dry-run --save-requests requests.jsonl

# 2. 完了まで待つ（SUCCEEDED なら終了コード0）
gembatch wait batches/abc123 --interval 60s

# 3. 結果取得
gembatch results batches/abc123 --out results.jsonl     # {"key","text","error"}
gembatch results batches/abc123 --out-dir out/          # key.txt を1件ずつ
gembatch results batches/abc123 --format csv --out results.csv

# 4. 失敗分だけ再投入
gembatch results batches/abc123 --failed-keys > failed.txt
gembatch create --dir scans/ --prompt '...' --only-keys failed.txt
```

入力モードは `--dir` / `--prompts`（1行1プロンプト） / `--input`（`{"key","request"}` のJSONL）のどれか1つ。`--limit N` で先頭N件だけ使えます。他に `list` `status` `cancel` `delete` `usage`。各コマンドの詳細は `gembatch <command> -h`。

| 終了コード | 意味 |
|---|---|
| 0 | 成功 |
| 1 | エラー（API・ネットワークなど） |
| 2 | 引数の誤り |
| 3 | ジョブが SUCCEEDED 以外で終了（FAILED / CANCELLED / EXPIRED）、または `run` で一部の要求が失敗 |
| 4 | `wait --timeout` 超過（ジョブはまだ実行中の可能性） |

## AIエージェントから使う

`skill/gembatch/SKILL.md` をエージェントのスキルとして登録します（Claude / Claude Code など）。エージェントは `gembatch usage --json` でフラグ一覧を読み取れます。

## 配布（メンテナ向け）

タグを push すると GitHub Actions + GoReleaser が5プラットフォーム分のバイナリを Releases に添付します。

```bash
git tag v0.1.0 && git push origin v0.1.0
```

## 開発

```bash
go vet ./... && go test -race ./...
```
