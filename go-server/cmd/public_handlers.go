package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	pb "modernrat-server/gen"
	storage "modernrat-server/internal/storage"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// RegisterUser はユーザー登録を処理する（認証不要）
func (s *server) RegisterUser(ctx context.Context, req *pb.RegisterUserRequest) (*pb.RegisterUserResponse, error) {
	if req.SystemInfo == nil {
		return &pb.RegisterUserResponse{
			Success: false,
			Message: "システム情報が必要です",
		}, status.Error(codes.InvalidArgument, "システム情報が必要です")
	}

	systemInfo := req.SystemInfo

	log.Printf("ユーザー登録リクエスト受信: IP=%s, Username=%s",
		systemInfo.GetIpAddress(), systemInfo.GetUsername())

	if systemInfo.GetIpAddress() == "" || systemInfo.GetUsername() == "" {
		return &pb.RegisterUserResponse{
			Success: false,
			Message: "IPアドレスとユーザー名が必要です",
		}, status.Error(codes.InvalidArgument, "IPアドレスとユーザー名が必要です")
	}

	const maxIDGenerationAttempts = 5

	now := time.Now().Unix()

	systemInfoBytes, err := proto.Marshal(systemInfo)
	if err != nil {
		log.Printf("SystemInfo のシリアライズに失敗: %v", err)
		return &pb.RegisterUserResponse{
			Success: false,
			Message: "内部エラーが発生しました",
		}, status.Error(codes.Internal, "内部エラーが発生しました")
	}

	var (
		userID    string
		insertErr error
	)

	record := storage.UserRecord{
		SystemInfo:   systemInfoBytes,
		RegisteredAt: now,
		LastSeen:     now,
		IsOnline:     true,
	}

	for attempt := 0; attempt < maxIDGenerationAttempts; attempt++ {
		userID = uuid.NewString()
		record.UserID = userID

		insertErr = s.users.Create(ctx, record)
		if insertErr == nil {
			break
		}

		if errors.Is(insertErr, storage.ErrUserExists) {
			log.Printf("ユーザーIDが衝突しました (attempt=%d id=%s)", attempt+1, userID)
			continue
		}

		log.Printf("ユーザーデータの保存に失敗: %v", insertErr)
		return &pb.RegisterUserResponse{
			Success: false,
			Message: "ユーザー登録に失敗しました",
		}, status.Error(codes.Internal, "ユーザー登録に失敗しました")
	}

	if insertErr != nil {
		log.Printf("UUID 衝突によりユーザー登録を中断しました")
		return &pb.RegisterUserResponse{
			Success: false,
			Message: "既に登録されています。しばらくしてから再試行してください",
		}, status.Error(codes.AlreadyExists, "ユーザーが既に存在します")
	}

	osName := ""
	if osInfo := systemInfo.GetOsInfo(); osInfo != nil {
		osName = osInfo.GetName()
	}
	cpuModel := ""
	if cpuInfo := systemInfo.GetCpuInfo(); cpuInfo != nil {
		cpuModel = cpuInfo.GetModel()
	}

	log.Printf("ユーザー登録完了: UserID=%s, OS=%s, CPU=%s",
		userID, osName, cpuModel)

	return &pb.RegisterUserResponse{
		Success:      true,
		UserId:       userID,
		Message:      "ユーザー登録が完了しました",
		RegisteredAt: now,
	}, nil
}

// GenerateAdminToken は管理者 JWT を発行する（認証不要）
func (s *server) GenerateAdminToken(ctx context.Context, req *pb.GenerateAdminTokenRequest) (*pb.GenerateAdminTokenResponse, error) {
	if len(req.GetPassword()) == 0 {
		return &pb.GenerateAdminTokenResponse{
			Success: false,
			Message: "パスワードが必要です",
		}, status.Error(codes.InvalidArgument, "パスワードが必要です")
	}

	if !secureCompare([]byte(req.GetPassword()), s.adminPassword) {
		return &pb.GenerateAdminTokenResponse{
			Success: false,
			Message: "認証に失敗しました",
		}, status.Error(codes.Unauthenticated, "認証に失敗しました")
	}

	ttl := req.GetTtlSeconds()
	if ttl <= 0 {
		ttl = 3600
	}

	now := time.Now()
	expiresAt := now.Add(time.Duration(ttl) * time.Second)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "admin",
		"iat": now.Unix(),
		"exp": expiresAt.Unix(),
	})

	tokenString, err := token.SignedString(s.jwtSecret)
	if err != nil {
		return &pb.GenerateAdminTokenResponse{
			Success: false,
			Message: "トークンの生成に失敗しました",
		}, status.Error(codes.Internal, "トークンの生成に失敗しました")
	}

	return &pb.GenerateAdminTokenResponse{
		Success:   true,
		Token:     tokenString,
		Message:   "トークンを発行しました",
		ExpiresAt: expiresAt.Unix(),
	}, nil
}
