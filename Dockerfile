# syntax=docker/dockerfile:1

# --- Build Stage ---
FROM golang:1.22.0-alpine AS builder

WORKDIR /app

# go.mod と go.sum をコピーして依存関係をダウンロード
COPY app/go.mod app/go.sum ./
RUN go mod download

# ソースコードをコピー
COPY app/ ./

# アプリケーションをビルド
# CGO_ENABLED=0 で静的リンクされたバイナリを生成し、scratchイメージでの実行を容易にする
# -ldflags="-s -w" でデバッグ情報を削除し、バイナリサイズを削減
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/lol-ranking-bot main.go

# --- Release Stage ---
FROM alpine:latest

WORKDIR /app

# ビルドステージから実行可能ファイルをコピー
COPY --from=builder /app/lol-ranking-bot /app/lol-ranking-bot

# .env ファイルはKoyebの環境変数で設定するため、Dockerfileには含めない

# アプリケーションがリッスンするポート (KoyebがPORT環境変数を設定)
# EXPOSEディレクティブはドキュメンテーション目的であり、実際にポートを開くのはKoyeb側の設定
# main.go内でos.Getenv("PORT")を読み取るようにしているので、ここで特定のポートを指定する必要はない
# EXPOSE 8080

# 実行コマンド
CMD ["/app/lol-ranking-bot"]
