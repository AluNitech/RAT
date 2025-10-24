#!/bin/bash

# Protocol Buffers コンパイル自動化スクリプト
# ModernRat プロジェクト用

set -e  # エラー時に終了

# カラー出力定義
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# プロジェクトルートディレクトリ
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROTO_DIR="$PROJECT_ROOT/proto"
GO_SERVER_PROTO_DIR="$PROJECT_ROOT/go-server/gen"
GO_CLIENT_PROTO_DIR="$PROJECT_ROOT/go-client/gen"

echo -e "${BLUE}=== Protocol Buffers コンパイル開始 ===${NC}"

# 必要なツールの確認
check_dependencies() {
    echo -e "${YELLOW}依存関係を確認中...${NC}"
    
    # protoc の確認
    if ! command -v protoc &> /dev/null; then
        echo -e "${RED}エラー: protoc が見つかりません${NC}"
        echo "インストール方法:"
        echo "  Ubuntu/Debian: sudo apt-get install protobuf-compiler"
        echo "  macOS: brew install protobuf"
        echo "  または https://protobuf.dev/downloads/ からダウンロード"
        exit 1
    fi
    
    # protoc-gen-go の確認
    if ! command -v protoc-gen-go &> /dev/null; then
        echo -e "${YELLOW}protoc-gen-go をインストールしています...${NC}"
        go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
    fi
    
    # protoc-gen-go-grpc の確認
    if ! command -v protoc-gen-go-grpc &> /dev/null; then
        echo -e "${YELLOW}protoc-gen-go-grpc をインストールしています...${NC}"
        go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
    fi
    
    echo -e "${GREEN}✓ 依存関係の確認完了${NC}"
}

# 出力ディレクトリの準備
prepare_output_dirs() {
    echo -e "${YELLOW}出力ディレクトリを準備中...${NC}"
    
    # サーバー用protoディレクトリを作成
    mkdir -p "$GO_SERVER_PROTO_DIR"
    
    # クライアント用protoディレクトリを作成
    mkdir -p "$GO_CLIENT_PROTO_DIR"
    
    echo -e "${GREEN}✓ 出力ディレクトリの準備完了${NC}"
}

# Go用のコード生成
compile_go() {
    echo -e "${YELLOW}Go用コードを生成中...${NC}"
    
    cd "$PROJECT_ROOT"
    
    # サーバー用のprotobufとgRPCコードを生成
    echo -e "${BLUE}  → サーバー用コード生成${NC}"
    protoc \
        --proto_path="$PROTO_DIR" \
        --go_out="$GO_SERVER_PROTO_DIR" \
        --go_opt=paths=source_relative \
        --go-grpc_out="$GO_SERVER_PROTO_DIR" \
        --go-grpc_opt=paths=source_relative \
        "$PROTO_DIR"/*.proto
    
    # クライアント用のprotobufとgRPCコードを生成
    echo -e "${BLUE}  → クライアント用コード生成${NC}"
    protoc \
        --proto_path="$PROTO_DIR" \
        --go_out="$GO_CLIENT_PROTO_DIR" \
        --go_opt=paths=source_relative \
        --go-grpc_out="$GO_CLIENT_PROTO_DIR" \
        --go-grpc_opt=paths=source_relative \
        "$PROTO_DIR"/*.proto
    
    echo -e "${GREEN}✓ Go用コード生成完了${NC}"
}

# Go modules の更新
update_go_modules() {
    echo -e "${YELLOW}Go modules を更新中...${NC}"
    
    # go-server のgo.mod更新
    if [ -f "$PROJECT_ROOT/go-server/go.mod" ]; then
        cd "$PROJECT_ROOT/go-server"
        go mod tidy
        echo -e "${BLUE}  → サーバーmodules更新完了${NC}"
    fi
    
    # go-client のgo.mod更新
    if [ -f "$PROJECT_ROOT/go-client/go.mod" ]; then
        cd "$PROJECT_ROOT/go-client"
        go mod tidy
        echo -e "${BLUE}  → クライアントmodules更新完了${NC}"
    fi
    
    echo -e "${GREEN}✓ Go modules 更新完了${NC}"
}

# 生成されたファイルの一覧表示
show_generated_files() {
    echo -e "${BLUE}=== 生成されたファイル ===${NC}"
    
    echo -e "${YELLOW}Server Proto Files:${NC}"
    if [ -d "$GO_SERVER_PROTO_DIR" ]; then
        find "$GO_SERVER_PROTO_DIR" -type f -name "*.go" | sort
    else
        echo "  なし"
    fi
    
    echo -e "${YELLOW}Client Proto Files:${NC}"
    if [ -d "$GO_CLIENT_PROTO_DIR" ]; then
        find "$GO_CLIENT_PROTO_DIR" -type f -name "*.go" | sort
    else
        echo "  なし"
    fi
}

# ヘルプ表示
show_help() {
    echo "使用方法: $0 [オプション]"
    echo ""
    echo "オプション:"
    echo "  --clean          生成ファイルをクリーン"
    echo "  --help           このヘルプを表示"
    echo ""
    echo "例:"
    echo "  $0               # Goコードを生成"
    echo "  $0 --clean       # 生成ファイルをクリーン"
}

# クリーン機能
clean_generated() {
    echo -e "${YELLOW}生成されたファイルをクリーン中...${NC}"
    
    if [ -d "$GO_SERVER_PROTO_DIR" ]; then
        rm -rf "$GO_SERVER_PROTO_DIR"
        echo -e "${BLUE}  → サーバー proto ファイルをクリーン${NC}"
    fi
    
    if [ -d "$GO_CLIENT_PROTO_DIR" ]; then
        rm -rf "$GO_CLIENT_PROTO_DIR"
        echo -e "${BLUE}  → クライアント proto ファイルをクリーン${NC}"
    fi
    
    echo -e "${GREEN}✓ クリーン完了${NC}"
}

# メイン処理
main() {
    # 引数の解析
    CLEAN_ONLY=false
    
    while [[ $# -gt 0 ]]; do
        case $1 in
            --clean)
                CLEAN_ONLY=true
                shift
                ;;
            --help)
                show_help
                exit 0
                ;;
            *)
                echo -e "${RED}未知のオプション: $1${NC}"
                show_help
                exit 1
                ;;
        esac
    done
    
    # クリーンのみの場合
    if [ "$CLEAN_ONLY" = true ]; then
        clean_generated
        exit 0
    fi
    
    # protoファイルの存在確認
    if [ ! -d "$PROTO_DIR" ] || [ -z "$(ls -A "$PROTO_DIR"/*.proto 2>/dev/null)" ]; then
        echo -e "${RED}エラー: protoファイルが見つかりません${NC}"
        echo "場所: $PROTO_DIR"
        exit 1
    fi
    
    # コンパイル実行
    check_dependencies
    prepare_output_dirs
    compile_go
    update_go_modules
    show_generated_files
    
    echo -e "${GREEN}=== コンパイル完了 ===${NC}"
    echo -e "${BLUE}Server Proto: $GO_SERVER_PROTO_DIR${NC}"
    echo -e "${BLUE}Client Proto: $GO_CLIENT_PROTO_DIR${NC}"
}

# スクリプト実行
main "$@"