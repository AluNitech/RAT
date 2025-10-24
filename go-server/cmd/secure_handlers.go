package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	pb "modernrat-server/gen"
	storage "modernrat-server/internal/storage"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const jwtAllowedSkew = 5 * time.Second

// authenticate は JWT を検証し、認証状態を確認する
func (s *server) authenticate(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "メタデータが提供されていません")
	}

	authHeaders := md.Get("authorization")
	if len(authHeaders) == 0 {
		return status.Error(codes.Unauthenticated, "Authorization ヘッダーがありません")
	}

	tokenString := authHeaders[0]
	if strings.HasPrefix(strings.ToLower(tokenString), "bearer ") {
		tokenString = tokenString[7:]
	}

	if tokenString == "" {
		return status.Error(codes.Unauthenticated, "JWT が空です")
	}

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("予期しない署名アルゴリズム: %v", t.Method.Alg())
		}
		return s.jwtSecret, nil
	})
	if err != nil || token == nil || !token.Valid {
		log.Printf("JWT 検証失敗: %v", err)
		return status.Error(codes.Unauthenticated, "トークンが無効です")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		log.Printf("JWT クレーム型が想定外です: %T", token.Claims)
		return status.Error(codes.Unauthenticated, "トークンが無効です")
	}

	if err := validateAdminClaims(claims, time.Now()); err != nil {
		log.Printf("JWT クレーム検証失敗: %v", err)
		return status.Error(codes.Unauthenticated, "トークンが無効です")
	}

	return nil
}

func validateAdminClaims(claims jwt.MapClaims, now time.Time) error {
	sub, _ := claims["sub"].(string)
	if sub != "admin" {
		return errors.New("unexpected subject claim")
	}

	exp, err := numericClaim(claims, "exp")
	if err != nil {
		return err
	}
	if now.After(time.Unix(exp, 0).Add(jwtAllowedSkew)) {
		return errors.New("token expired")
	}

	if nbf, err := numericClaim(claims, "nbf"); err == nil && now.Add(jwtAllowedSkew).Before(time.Unix(nbf, 0)) {
		return errors.New("token not yet valid")
	}

	if iat, err := numericClaim(claims, "iat"); err == nil && time.Unix(iat, 0).After(now.Add(jwtAllowedSkew)) {
		return errors.New("token issued in the future")
	}

	if audValue, exists := claims["aud"]; exists {
		switch aud := audValue.(type) {
		case string:
			if aud == "" {
				return errors.New("audience claim empty")
			}
		case []interface{}:
			if len(aud) == 0 {
				return errors.New("audience claim empty")
			}
		default:
			return errors.New("audience claim has unexpected type")
		}
	}

	return nil
}

func numericClaim(claims jwt.MapClaims, key string) (int64, error) {
	value, ok := claims[key]
	if !ok {
		return 0, fmt.Errorf("%s claim missing", key)
	}

	switch v := value.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s claim invalid: %w", key, err)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("%s claim type unsupported: %T", key, value)
	}
}

// GetUser は指定されたユーザーIDの情報を取得する（認証必須）
func (s *server) GetUser(ctx context.Context, req *pb.GetUserRequest) (*pb.GetUserResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}

	log.Printf("ユーザー情報取得リクエスト: UserID=%s", req.UserId)

	if req.UserId == "" {
		return &pb.GetUserResponse{
			Success: false,
			Message: "ユーザーIDが必要です",
		}, status.Error(codes.InvalidArgument, "ユーザーIDが必要です")
	}

	var (
		systemInfoBytes []byte
		registeredAt    int64
		lastSeen        int64
		isOnlineInt     int64
	)

	err := s.db.QueryRowContext(ctx, `
        SELECT system_info, registered_at, last_seen, is_online
        FROM users
        WHERE user_id = ?`,
		req.UserId,
	).Scan(&systemInfoBytes, &registeredAt, &lastSeen, &isOnlineInt)
	if err == sql.ErrNoRows {
		return &pb.GetUserResponse{
			Success: false,
			Message: "ユーザーが見つかりません",
		}, status.Error(codes.NotFound, "ユーザーが見つかりません")
	}
	if err != nil {
		log.Printf("ユーザーデータの取得に失敗: %v", err)
		return &pb.GetUserResponse{
			Success: false,
			Message: "ユーザー情報の取得に失敗しました",
		}, status.Error(codes.Internal, "ユーザー情報の取得に失敗しました")
	}

	systemInfo := &pb.SystemInfo{}
	if err := proto.Unmarshal(systemInfoBytes, systemInfo); err != nil {
		log.Printf("SystemInfo のデシリアライズに失敗: %v", err)
		return &pb.GetUserResponse{
			Success: false,
			Message: "ユーザーデータの読み込みに失敗しました",
		}, status.Error(codes.Internal, "ユーザーデータの読み込みに失敗しました")
	}

	// Update last_seen for activity, but do NOT forcibly set is_online to true here.
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET last_seen = ?
		WHERE user_id = ?`,
		now, req.UserId,
	); err != nil {
		log.Printf("ユーザーデータの更新に失敗: %v", err)
		return &pb.GetUserResponse{
			Success: false,
			Message: "ユーザーデータの更新に失敗しました",
		}, status.Error(codes.Internal, "ユーザーデータの更新に失敗しました")
	}
	lastSeen = now

	return &pb.GetUserResponse{
		Success:      true,
		UserId:       req.UserId,
		SystemInfo:   systemInfo,
		RegisteredAt: registeredAt,
		LastSeen:     lastSeen,
		Message:      "ユーザー情報を取得しました",
	}, nil
}

// ListUsers は登録されたユーザーのリストを取得する（認証必須）
func (s *server) ListUsers(ctx context.Context, req *pb.ListUsersRequest) (*pb.ListUsersResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}

	log.Printf("ユーザーリスト取得リクエスト: Page=%d, PageSize=%d, Filter=%s", req.Page, req.PageSize, req.Filter)

	page := int(req.GetPage())
	if page <= 0 {
		page = 1
	}
	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = 10
	}

	filterInput := strings.TrimSpace(req.GetFilter())
	whereClauses := make([]string, 0)
	args := make([]interface{}, 0)

	addCondition := func(condition string, values ...interface{}) {
		whereClauses = append(whereClauses, condition)
		args = append(args, values...)
	}

	if filterInput != "" {
		for _, token := range strings.Split(filterInput, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}

			raw := token
			lower := strings.ToLower(token)

			switch {
			case lower == "online", lower == "status:online", lower == "status=online":
				addCondition("is_online = 1")
			case lower == "offline", lower == "status:offline", lower == "status=offline":
				addCondition("is_online = 0")
			case strings.HasPrefix(lower, "status:"):
				val := strings.TrimSpace(lower[len("status:"):])
				if val == "online" {
					addCondition("is_online = 1")
				} else if val == "offline" {
					addCondition("is_online = 0")
				}
			case strings.HasPrefix(lower, "user:"):
				term := strings.TrimSpace(raw[len("user:"):])
				if term != "" {
					like := "%" + term + "%"
					addCondition("user_id LIKE ?", like)
				}
			case strings.HasPrefix(lower, "id:"):
				term := strings.TrimSpace(raw[len("id:"):])
				if term != "" {
					like := "%" + term + "%"
					addCondition("user_id LIKE ?", like)
				}
			default:
				like := "%" + raw + "%"
				addCondition("user_id LIKE ?", like)
			}
		}
	}

	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM users %s`, whereClause)

	var totalCount int64
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		log.Printf("ユーザー総数の取得に失敗: %v", err)
		return &pb.ListUsersResponse{
			Success: false,
			Message: "ユーザーリストの取得に失敗しました",
		}, status.Error(codes.Internal, "ユーザーリストの取得に失敗しました")
	}

	if totalCount == 0 {
		page = 1
	} else {
		maxPage := int((totalCount + int64(pageSize) - 1) / int64(pageSize))
		if maxPage < 1 {
			maxPage = 1
		}
		if page > maxPage {
			page = maxPage
		}
	}

	offset := (page - 1) * pageSize
	query := fmt.Sprintf(`
	        SELECT user_id, system_info, registered_at, last_seen, is_online
	        FROM users
	        %s
	        ORDER BY registered_at DESC
	        LIMIT ? OFFSET ?`, whereClause)

	queryArgs := append(append([]interface{}{}, args...), pageSize, offset)
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		log.Printf("ユーザーリスト取得クエリに失敗: %v", err)
		return &pb.ListUsersResponse{
			Success: false,
			Message: "ユーザーリストの取得に失敗しました",
		}, status.Error(codes.Internal, "ユーザーリストの取得に失敗しました")
	}
	defer rows.Close()

	var userSummaries []*pb.UserSummary
	for rows.Next() {
		var (
			userID         string
			systemInfoData []byte
			registeredAt   int64
			lastSeen       int64
			isOnlineInt    int64
		)

		if err := rows.Scan(&userID, &systemInfoData, &registeredAt, &lastSeen, &isOnlineInt); err != nil {
			log.Printf("ユーザーリストのスキャンに失敗: %v", err)
			return &pb.ListUsersResponse{
				Success: false,
				Message: "ユーザーリストの読み込みに失敗しました",
			}, status.Error(codes.Internal, "ユーザーリストの読み込みに失敗しました")
		}

		systemInfo := &pb.SystemInfo{}
		if err := proto.Unmarshal(systemInfoData, systemInfo); err != nil {
			log.Printf("SystemInfo のデシリアライズに失敗: %v", err)
			return &pb.ListUsersResponse{
				Success: false,
				Message: "ユーザーリストの読み込みに失敗しました",
			}, status.Error(codes.Internal, "ユーザーリストの読み込みに失敗しました")
		}

		summary := &pb.UserSummary{
			UserId:       userID,
			Username:     systemInfo.GetUsername(),
			IpAddress:    systemInfo.GetIpAddress(),
			Hostname:     systemInfo.GetHostname(),
			RegisteredAt: registeredAt,
			LastSeen:     lastSeen,
			IsOnline:     isOnlineInt != 0,
		}

		if osInfo := systemInfo.GetOsInfo(); osInfo != nil {
			summary.OsName = osInfo.GetName()
		}

		userSummaries = append(userSummaries, summary)
	}

	if err := rows.Err(); err != nil {
		log.Printf("ユーザーリストの読み込み中にエラー発生: %v", err)
		return &pb.ListUsersResponse{
			Success: false,
			Message: "ユーザーリストの読み込みに失敗しました",
		}, status.Error(codes.Internal, "ユーザーリストの読み込みに失敗しました")
	}

	log.Printf("ユーザーリスト取得完了: TotalCount=%d, ReturnedCount=%d", totalCount, len(userSummaries))

	return &pb.ListUsersResponse{
		Success:    true,
		Users:      userSummaries,
		TotalCount: int32(totalCount),
		Page:       int32(page),
		PageSize:   int32(pageSize),
		Message:    "ユーザーリストを取得しました",
	}, nil
}

func (s *server) DeleteUser(ctx context.Context, req *pb.DeleteUserRequest) (*pb.DeleteUserResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}

	userID := strings.TrimSpace(req.GetUserId())
	if userID == "" {
		return &pb.DeleteUserResponse{
			Success: false,
			Message: "ユーザーIDを指定してください",
		}, status.Error(codes.InvalidArgument, "user_id is required")
	}

	reason := "user deleted by admin"

	shellConn, shellSessions := s.shellHub.disconnectUser(userID)
	for _, session := range shellSessions {
		s.broadcastSessionClose(session, reason)
	}
	if shellConn != nil {
		shellConn.Close()
	}

	fileConn, fileSessions := s.fileHub.disconnectUser(userID)
	for _, session := range fileSessions {
		cancelMsg := &pb.FileTransferMessage{
			TransferId: session.id,
			UserId:     session.userID,
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL,
			Text:       reason,
		}
		if session.adminConn != nil {
			_ = session.adminConn.Send(cancelMsg)
		}
		if session.clientConn != nil {
			_ = session.clientConn.Send(cancelMsg)
		}
	}
	if fileConn != nil {
		fileConn.Close()
	}

	if err := s.users.Delete(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrUserNotFound) {
			return &pb.DeleteUserResponse{
				Success: false,
				Message: "ユーザーが見つかりません",
				UserId:  userID,
			}, status.Error(codes.NotFound, "user not found")
		}
		log.Printf("ユーザー削除に失敗しました: user=%s err=%v", userID, err)
		return &pb.DeleteUserResponse{
			Success: false,
			Message: "ユーザー削除に失敗しました",
			UserId:  userID,
		}, status.Error(codes.Internal, "failed to delete user")
	}

	log.Printf("ユーザー削除完了: user=%s", userID)
	return &pb.DeleteUserResponse{
		Success: true,
		Message: "ユーザーを削除しました",
		UserId:  userID,
	}, nil
}
