package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

var (
	ErrUserExists   = errors.New("user already exists")
	ErrUserNotFound = errors.New("user not found")
)

// UserRecord represents a user row persisted in the database.
type UserRecord struct {
	UserID           string
	SystemInfo       []byte
	RegisteredAt     int64
	LastSeen         int64
	IsOnline         bool
	ClientSecretHash string
}

// UserRepository defines persistence operations for users.
type UserRepository interface {
	Create(ctx context.Context, user UserRecord) error
	GetByID(ctx context.Context, userID string) (UserRecord, error)
	UpdateRegistration(ctx context.Context, user UserRecord, updateSecret bool) error
	Delete(ctx context.Context, userID string) error
}

// SQLiteUserRepository is a concrete UserRepository backed by SQLite.
type SQLiteUserRepository struct {
	db *sql.DB
}

// NewSQLiteUserRepository builds a new SQLiteUserRepository instance.
func NewSQLiteUserRepository(db *sql.DB) *SQLiteUserRepository {
	return &SQLiteUserRepository{db: db}
}

// Create inserts a new user row, returning ErrUserExists when the user already exists.
func (r *SQLiteUserRepository) Create(ctx context.Context, user UserRecord) error {
	if user.RegisteredAt == 0 {
		user.RegisteredAt = time.Now().Unix()
	}
	if user.LastSeen == 0 {
		user.LastSeen = user.RegisteredAt
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users (user_id, system_info, registered_at, last_seen, is_online, client_secret_hash) VALUES (?, ?, ?, ?, ?, ?)`,
		user.UserID,
		user.SystemInfo,
		user.RegisteredAt,
		user.LastSeen,
		boolToInt(user.IsOnline),
		user.ClientSecretHash,
	)
	if err != nil {
		if sqliteIsConstraintErr(err) {
			return ErrUserExists
		}
		return err
	}
	return nil
}

func (r *SQLiteUserRepository) GetByID(ctx context.Context, userID string) (UserRecord, error) {
	var (
		record UserRecord
		online int
	)
	row := r.db.QueryRowContext(ctx,
		`SELECT user_id, system_info, registered_at, last_seen, is_online, client_secret_hash FROM users WHERE user_id = ?`,
		userID,
	)
	if err := row.Scan(&record.UserID, &record.SystemInfo, &record.RegisteredAt, &record.LastSeen, &online, &record.ClientSecretHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, ErrUserNotFound
		}
		return UserRecord{}, err
	}
	record.IsOnline = online != 0
	return record, nil
}

func (r *SQLiteUserRepository) UpdateRegistration(ctx context.Context, user UserRecord, updateSecret bool) error {
	if user.UserID == "" {
		return errors.New("user id is required")
	}
	query := `UPDATE users SET system_info = ?, last_seen = ?, is_online = ? WHERE user_id = ?`
	args := []any{user.SystemInfo, user.LastSeen, boolToInt(user.IsOnline), user.UserID}
	if updateSecret {
		query = `UPDATE users SET system_info = ?, last_seen = ?, is_online = ?, client_secret_hash = ? WHERE user_id = ?`
		args = []any{user.SystemInfo, user.LastSeen, boolToInt(user.IsOnline), user.ClientSecretHash, user.UserID}
	}
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (r *SQLiteUserRepository) Delete(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("user id is required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE user_id = ?`, strings.TrimSpace(userID))
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return ErrUserNotFound
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sqliteIsConstraintErr(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code == sqlite3.ErrConstraint && (sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey || sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique) {
			return true
		}
		return false
	}

	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unique constraint") && strings.Contains(lower, "user")
}
