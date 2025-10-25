# ModernRat プロジェクト用 Makefile

BIN_ROOT := bin
BIN_LINUX := $(BIN_ROOT)/linux
BIN_WINDOWS := $(BIN_ROOT)/windows
FFMPEG_ROOT := third_party/ffmpeg
FFMPEG_LINUX_BIN := $(FFMPEG_ROOT)/linux/ffmpeg
FFPLAY_LINUX_BIN := $(FFMPEG_ROOT)/linux/ffplay
FFMPEG_WINDOWS_BIN := $(FFMPEG_ROOT)/windows/ffmpeg.exe
FFPLAY_WINDOWS_BIN := $(FFMPEG_ROOT)/windows/ffplay.exe

.PHONY: proto proto-clean help build-server build-client build-adminshell run-server run-client run-adminshell clean install-deps \
	build-server-windows build-client-windows build-adminshell-windows build-all-windows build-all-platforms \
	bundle-client-linux bundle-client-windows bundle-adminshell-linux bundle-adminshell-windows

# デフォルトターゲット
help:
	@echo "利用可能なコマンド:"
	@echo "  proto         - Go用protoファイルをコンパイル"
	@echo "  proto-clean   - 生成されたprotoファイルをクリーン"
	@echo "  build-server  - Go サーバーをビルド (bin/linux/server)"
	@echo "  build-client  - Go クライアント(エージェント)をビルド (bin/linux/client)"
	@echo "  build-adminshell - 管理者用シェルCLIをビルド (bin/linux/adminshell)"
	@echo "  build-server-windows   - Windows 用サーバーバイナリをビルド (bin/windows/server.exe)"
	@echo "  build-client-windows   - Windows 用クライアントバイナリをビルド (bin/windows/client.exe)"
	@echo "  build-adminshell-windows - Windows 用管理者CLIをビルド (bin/windows/adminshell.exe)"
	@echo "  build-all-platforms - すべてのプラットフォーム向けバイナリをまとめてビルド"
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
	cd go-server && go build -o ../$(BIN_LINUX)/server ./cmd

build-client: bin
	@echo "Go クライアントをビルド中..."
	cd go-client && go build -o ../$(BIN_LINUX)/client ./cmd
	@$(MAKE) bundle-client-linux

build-adminshell: bin
	@echo "管理者用シェルCLIをビルド中..."
	cd go-adminshell && go build -o ../$(BIN_LINUX)/adminshell ./cmd/adminshell
	@$(MAKE) bundle-adminshell-linux

# Windows 向けクロスビルド
build-server-windows: bin
	@echo "Windows 用サーバーをビルド中..."
	cd go-server && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../$(BIN_WINDOWS)/server.exe ./cmd

build-client-windows: bin
	@echo "Windows 用クライアントをビルド中..."
	cd go-client && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../$(BIN_WINDOWS)/client.exe ./cmd
	@$(MAKE) bundle-client-windows

build-adminshell-windows: bin
	@echo "Windows 用管理者用シェルCLIをビルド中..."
	cd go-adminshell && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../$(BIN_WINDOWS)/adminshell.exe ./cmd/adminshell
	@$(MAKE) bundle-adminshell-windows

# 実行関連
run-server: build-server
	@echo "Go サーバーを実行中..."
	./$(BIN_LINUX)/server

run-client: build-client
	@echo "Go クライアントを実行中..."
	./$(BIN_LINUX)/client

run-adminshell: build-adminshell
	@echo "管理者用シェルCLIを実行中..."
	./$(BIN_LINUX)/adminshell

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

build-all-platforms: proto build-server build-client build-adminshell build-client-windows build-adminshell-windows
	@echo "すべてのプラットフォーム向けバイナリのビルド完了 (Windows サーバーは除外)"

# バイナリディレクトリ作成
bin:
	mkdir -p $(BIN_LINUX) $(BIN_WINDOWS)

bundle-client-linux:
	@test -f $(FFMPEG_LINUX_BIN) || (echo "Missing FFmpeg binary: $(FFMPEG_LINUX_BIN)" && exit 1)
	@install -m 755 $(FFMPEG_LINUX_BIN) $(BIN_LINUX)/ffmpeg

bundle-adminshell-linux:
	@test -f $(FFPLAY_LINUX_BIN) || (echo "Missing FFplay binary: $(FFPLAY_LINUX_BIN)" && exit 1)
	@install -m 755 $(FFPLAY_LINUX_BIN) $(BIN_LINUX)/ffplay

bundle-client-windows:
	@test -f $(FFMPEG_WINDOWS_BIN) || (echo "Missing FFmpeg binary: $(FFMPEG_WINDOWS_BIN)" && exit 1)
	@cp $(FFMPEG_WINDOWS_BIN) $(BIN_WINDOWS)/ffmpeg.exe

bundle-adminshell-windows:
	@test -f $(FFPLAY_WINDOWS_BIN) || (echo "Missing FFplay binary: $(FFPLAY_WINDOWS_BIN)" && exit 1)
	@cp $(FFPLAY_WINDOWS_BIN) $(BIN_WINDOWS)/ffplay.exe