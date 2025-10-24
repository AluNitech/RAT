# ModernRat プロジェクト用 Makefile

.PHONY: proto proto-clean help build-server build-client build-adminshell run-server run-client run-adminshell clean install-deps \
	build-server-windows build-client-windows build-adminshell-windows build-all-windows

# デフォルトターゲット
help:
	@echo "利用可能なコマンド:"
	@echo "  proto         - Go用protoファイルをコンパイル"
	@echo "  proto-clean   - 生成されたprotoファイルをクリーン"
	@echo "  build-server  - Go サーバーをビルド"
	@echo "  build-client  - Go クライアント(エージェント)をビルド"
	@echo "  build-adminshell - 管理者用シェルCLIをビルド"
	@echo "  build-server-windows   - Windows 用サーバーバイナリをビルド (server.exe)"
	@echo "  build-client-windows   - Windows 用クライアントバイナリをビルド (client.exe)"
	@echo "  build-adminshell-windows - Windows 用管理者CLIをビルド (adminshell.exe)"
	@echo "  run-server    - Go サーバーを実行"
	@echo "  run-client    - Go クライアント(エージェント)を実行"
	@echo "  run-adminshell - 管理者用シェルCLIを実行"
	@echo "  install-deps  - 必要な依存関係をインストール"
	@echo "  clean         - すべてのビルド成果物をクリーン"

# Protocol Buffers関連
proto:
	@echo "Protocol Buffers コンパイル中..."
	./compile_proto.sh

proto-clean:
	@echo "生成されたファイルをクリーン中..."
	./compile_proto.sh --clean

# Goビルド関連
build-server: bin
	@echo "Go サーバーをビルド中..."
	cd go-server && go build -o ../bin/server ./cmd

build-client: bin
	@echo "Go クライアントをビルド中..."
	cd go-client && go build -o ../bin/client ./cmd

build-adminshell: bin
	@echo "管理者用シェルCLIをビルド中..."
	cd go-adminshell && go build -o ../bin/adminshell ./cmd/adminshell

# Windows 向けクロスビルド
build-server-windows: bin
	@echo "Windows 用サーバーをビルド中..."
	cd go-server && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/server.exe ./cmd

build-client-windows: bin
	@echo "Windows 用クライアントをビルド中..."
	cd go-client && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/client.exe ./cmd

build-adminshell-windows: bin
	@echo "Windows 用管理者用シェルCLIをビルド中..."
	cd go-adminshell && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/adminshell.exe ./cmd/adminshell

# 実行関連
run-server: build-server
	@echo "Go サーバーを実行中..."
	./bin/server

run-client: build-client
	@echo "Go クライアントを実行中..."
	./bin/client

run-adminshell: build-adminshell
	@echo "管理者用シェルCLIを実行中..."
	./bin/adminshell

# 依存関係インストール
install-deps:
	@echo "必要な依存関係をインストール中..."
	@echo "Protocol Buffers コンパイラをチェック..."
	@which protoc > /dev/null || (echo "protoc をインストールしてください" && exit 1)
	@echo "Go依存関係をインストール..."
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "依存関係のインストール完了"

# クリーンアップ
clean: proto-clean
	@echo "ビルド成果物をクリーン中..."
	rm -rf bin/
	cd go-server && go clean
	cd go-client && go clean
	cd go-adminshell && go clean
	@echo "クリーン完了"

# 開発用ショートカット
dev-setup: install-deps proto
	@echo "開発環境セットアップ完了"

# プロジェクト全体ビルド
build-all: proto build-server build-client build-adminshell
	@echo "プロジェクト全体のビルド完了"

build-all-windows: proto build-server-windows build-client-windows build-adminshell-windows
	@echo "Windows 向けプロジェクト全体のビルド完了"

# バイナリディレクトリ作成
bin:
	mkdir -p bin