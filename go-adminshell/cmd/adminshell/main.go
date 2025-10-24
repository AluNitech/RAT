package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "modernrat-client/gen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultAddress      = "localhost:50051"
	defaultListPageSize = 50
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	addr := flag.String("addr", defaultAddress, "gRPC server address")
	password := flag.String("password", "", "admin password (use with caution)")
	passwordFile := flag.String("password-file", "", "path to file containing the admin password")
	token := flag.String("token", "", "existing admin bearer token")
	tokenFile := flag.String("token-file", "", "path to file containing an admin token")
	tokenTTL := flag.Duration("token-ttl", 0, "requested admin token TTL (e.g. 1h, 30m)")
	defaultUser := flag.String("user", "", "set initial attached user")
	flag.Parse()

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.DialContext(rootCtx, *addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		log.Fatalf("サーバー接続に失敗しました: %v", err)
	}
	defer conn.Close()

	adminClient := pb.NewAdminServiceClient(conn)
	shellClient := pb.NewRemoteShellServiceClient(conn)
	fileClient := pb.NewFileTransferServiceClient(conn)

	adminToken := strings.TrimSpace(*token)
	if adminToken == "" && strings.TrimSpace(*tokenFile) != "" {
		if value, loadErr := loadTrimmed(*tokenFile); loadErr != nil {
			log.Fatalf("トークンファイルの読み込みに失敗しました: %v", loadErr)
		} else {
			adminToken = value
		}
	}

	var expiresAt time.Time
	if adminToken == "" {
		pwd := strings.TrimSpace(*password)
		if pwd == "" && strings.TrimSpace(*passwordFile) != "" {
			if value, loadErr := loadTrimmed(*passwordFile); loadErr != nil {
				log.Fatalf("パスワードファイルの読み込みに失敗しました: %v", loadErr)
			} else {
				pwd = value
			}
		}
		if pwd == "" {
			var readErr error
			pwd, readErr = readSecret("Admin password: ")
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					log.Fatal("パスワード入力がキャンセルされました")
				}
				log.Fatalf("パスワードの読み取りに失敗しました: %v", readErr)
			}
		}
		if pwd == "" {
			log.Fatal("パスワードが空です")
		}

		tokenValue, exp, reqErr := requestAdminToken(rootCtx, adminClient, pwd, *tokenTTL)
		if reqErr != nil {
			log.Fatalf("管理トークンの取得に失敗しました: %v", reqErr)
		}
		adminToken = tokenValue
		expiresAt = exp
		if expiresAt.IsZero() {
			log.Println("新しい管理トークンを取得しました (有効期限なし)")
		} else {
			log.Printf("新しい管理トークンを取得しました (有効期限 %s)", expiresAt.Local().Format(time.RFC3339))
		}
	}

	if adminToken == "" {
		log.Fatal("管理トークンを取得できませんでした")
	}

	app := &adminApp{
		ctx:          rootCtx,
		adminClient:  adminClient,
		shellClient:  shellClient,
		fileClient:   fileClient,
		token:        adminToken,
		defaultUser:  strings.TrimSpace(*defaultUser),
		attachedUser: "",
		listFilter:   "",
		listPageSize: defaultListPageSize,
	}
	if app.defaultUser != "" {
		app.attachedUser = app.defaultUser
	}

	if err := app.run(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		log.Fatalf("admin shell の実行に失敗しました: %v", err)
	}
}
