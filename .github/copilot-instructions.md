# ModernRat AI コーディングガイド

## アーキテクチャ概要
- `go-server/cmd/main.go` が gRPC サーバーを起動し、`public_handlers.go` で `RegisterUser`/`GenerateAdminToken` を、`remote_shell_handlers.go`・`file_transfer_handlers.go`・`screen_capture_handlers.go` で管理／クライアント双方のストリーミング RPC を実装します。
- `go-client/cmd/main.go` は OS 情報を収集して登録し、`runRemoteShellClient`・`runFileTransferClient`・`runScreenCaptureClient` をそれぞれ goroutine で常駐させます。ストリームは全て `context.Context` と指数バックオフで再接続します。
- `go-adminshell/cmd/adminshell/app.go` 配下は readline ベースの CLI。`auth.go` で `GenerateAdminToken` を呼び、`session.go`/`file_transfer.go`/`capture.go` で gRPC ストリームを端末 I/O に橋渡しします。
- gRPC プロトコルは `proto/*.proto` にまとまり、`./compile_proto.sh`（=`make proto`）が `go-server/gen` と `go-client/gen` を同時に生成＋`go mod tidy`。生成物は両モジュールと `go-adminshell/go.mod` の `replace modernrat-client => ../go-client` を壊さないようコミットします。

## ビルド / 実行 / テスト
- ルート `Makefile` で `make build-server|client|adminshell`（Linux 出力は `bin/linux/*`）。Windows 向けは各 `build-*-windows` ターゲットが `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` でクロスビルドし、`third_party/ffmpeg/**` から FFmpeg/FFplay をバンドルします。
- サーバー起動には最低 `JWT_SECRET` と `ADMIN_PASSWORD`、必要に応じ `TLS_CERT_FILE`, `TLS_KEY_FILE`, `SERVER_LISTEN_ADDR`, `DB_PATH` を `./bin/linux/server` 実行前にエクスポートしてください（`make run-server` でビルド＋起動可能）。
- エージェント／管理 CLI は `./bin/linux/{client,adminshell}` か `go run ./cmd[...]` で動作し、FFmpeg/FFplay が見つからないとスクリーンキャプチャ／再生が失敗します。
- テストは現在 `go-server/cmd/server_integration_test.go` のみ。`cd go-server && go test ./...` を基準に追加テストを配置します（SQLite は `file::memory:` で初期化）。

## サーバー実装パターン
- 認証が必要な RPC は必ず `(*server).authenticate`（`secure_handlers.go`）を冒頭で呼び、`authorization: Bearer <JWT>` メタデータを検証します。JWT 生成は `GenerateAdminToken` に一本化してください。
- 接続状態は `shell_hub.go`・`file_hub.go`・`screen_capture_hub.go` のハブでトラッキングします。セッションを手動でマップ操作せず、`startSession`/`endSession`/`registerClient` を経由するのが前提です。
- ユーザーデータベースは `internal/storage/user_repository.go` 経由で操作し、カラムを増やす際は `ensureSchema`/`ensureColumnExists` と `UserRecord` を同時更新します。サーバー終了時は `shutdownShellSessions` などで `is_online` をリセットします。
- ストリーミング実装は `append([]byte(nil), ...)` や `protoCloneCaptureMessage` でバッファをコピーし、共有メモリの race を避けます。新しいメッセージ型も同じパターンに従ってください。

## クライアント（エージェント）での注意
- `runRemoteShellClient` は `go-pty` で OS ごとのデフォルトシェルを開き、5 秒間隔の `HEARTBEAT` を送信してサーバーの `last_seen` を更新します。セッション制御は `clientShellSession` の `resize`/`stop` を経由します。
- `file_transfer_client.go` は 64KiB チャンクを基本に `upload`/`download` を切り替えるため、モード判定や `cleanupTransfer` の呼び出し順序を崩さないでください。
- `screen_capture_client.go` は `resolveFFmpegPath` → `buildFFmpegInputArgs/OutputArgs` で ffmpeg コマンドラインを構築し `pipe:1` を管理します。環境変数 `MODERNRAT_CAPTURE_SOURCE` / `MODERNRAT_WEBCAM_DEVICE` で入力ソースを上書きできます。
- `internal/identity/identity.go` は OS キーリングを優先し、`ErrBackendUnavailable` の場合のみ `~/.config/modernrat/credentials.json` へフォールバックします。資格情報を扱う変更ではこの挙動を維持してください。

## 管理 CLI / バンドル資産
- `go-adminshell/cmd/adminshell` は `readline` UI（`app.go` & `ui_colors.go`）でコマンドディスパッチ。新しいサブコマンドは `app.go` のコマンドテーブルと `help` 表示を同時に更新します。
- スクリーンキャプチャ再生は `ffplay` に依存します。`make build-adminshell`/`bundle-adminshell-*` が `third_party/ffmpeg/{linux,windows}/ffplay` を `bin/**/ffplay` にコピーするため、CI ではダミーバイナリを配置するかバンドルターゲットをスキップする仕組みが必要です。

## 運用上のヒント
- ログは `log.Printf("<component>: user=%s session=%s ...")` 形式で ID を残しており、追加のログも同じ粒度で揃えるとデバッグが容易です。
- サーバーの公開 API は gRPC のみなので、新しい HTTP エンドポイントを足す代わりに RPC を増やす方が既存ツールチェーン（`go-adminshell`・`go-client`）との整合性を取りやすいです。
- CI やローカル自動化で `make build-*` を呼ぶ場合は `third_party/ffmpeg` の存在を事前にチェックし、足りない場合はエラーをユーザーに伝えてください。
- 何らかのコード／設定変更を行う際は、実作業に入る前にユーザーへ目的と主要ステップを簡潔に共有し、了承を得てから進めてください。
