# lol-ranking

League of Legendsのランキング情報を表示するアプリケーションです。

## 概要

このアプリケーションは、League of Legendsのプレイヤーランキングや統計情報を取得し、表示することを目的としています。

## セットアップ

1. リポジトリをクローンします。
   ```bash
   git clone https://github.com/あなたのユーザー名/lol-ranking.git
   cd lol-ranking
   ```
2. 必要な環境変数を`.env`ファイルに設定します。`.env.example`を参考にしてください
3. 依存関係をインストールします。
   ```bash
   cd app
   go mod tidy
   ```
4. アプリケーションを実行します。
   ```bash
   go run main.go
   ```

## 使い方

アプリケーションを起動後、指定されたURLにアクセスしてランキング情報を確認できます。

## コントリビュート

貢献を歓迎します！プルリクエストを送るか、イシューを作成してください。
