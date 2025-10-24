package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/mattn/go-sqlite3"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testJWTSecret = "test-secret-key"

func newTestServer(t *testing.T) (*server, *sql.DB) {
	t.Helper()

	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}

	ctx := context.Background()
	if err := ensureSchema(ctx, db); err != nil {
		t.Fatalf("failed to ensure schema: %v", err)
	}

	cfg := serverConfig{
		listenAddr:    ":0",
		databasePath:  "file::memory:",
		jwtSecret:     []byte(testJWTSecret),
		adminPassword: []byte("pass"),
	}

	srv := newServerInstance(db, cfg)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return srv, db
}

func TestAuthenticateAcceptsValidAdminToken(t *testing.T) {
	srv, _ := newTestServer(t)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin",
		"exp": now.Add(1 * time.Minute).Unix(),
		"iat": now.Add(-1 * time.Minute).Unix(),
	})

	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+signed))
	if err := srv.authenticate(ctx); err != nil {
		t.Fatalf("authenticate returned error for valid token: %v", err)
	}
}

func TestAuthenticateRejectsExpiredToken(t *testing.T) {
	srv, _ := newTestServer(t)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin",
		"exp": now.Add(-1 * time.Minute).Unix(),
		"iat": now.Add(-2 * time.Minute).Unix(),
	})

	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+signed))
	err = srv.authenticate(ctx)
	if err == nil {
		t.Fatalf("expected error for expired token")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestAuthenticateRejectsWrongSubject(t *testing.T) {
	srv, _ := newTestServer(t)

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user",
		"exp": now.Add(1 * time.Minute).Unix(),
		"iat": now.Add(-1 * time.Minute).Unix(),
	})

	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+signed))
	err = srv.authenticate(ctx)
	if err == nil {
		t.Fatalf("expected error for wrong subject")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestShutdownShellSessionsMarksUsersOffline(t *testing.T) {
	srv, db := newTestServer(t)

	ctx := context.Background()
	now := time.Now().Unix()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (user_id, system_info, registered_at, last_seen, is_online) VALUES (?, ?, ?, ?, 1)`,
		"user-1", []byte("info"), now, now,
	)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	srv.shellHub.sessions["session-1"] = &shellSession{
		id:     "session-1",
		userID: "user-1",
	}

	srv.shutdownShellSessions("server shutting down")

	if len(srv.shellHub.sessions) != 0 {
		t.Fatalf("expected sessions to be cleared, got %d", len(srv.shellHub.sessions))
	}

	var isOnline int
	if err := db.QueryRowContext(ctx, `SELECT is_online FROM users WHERE user_id = ?`, "user-1").Scan(&isOnline); err != nil {
		t.Fatalf("failed to query user state: %v", err)
	}
	if isOnline != 0 {
		t.Fatalf("expected user to be marked offline, got %d", isOnline)
	}
}
