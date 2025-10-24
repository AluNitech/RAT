package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

var ErrUserExists = errors.New("user already exists")

// UserRecord represents a user row persisted in the database.
type UserRecord struct {
	UserID       string
	SystemInfo   []byte
	RegisteredAt int64
	LastSeen     int64
	IsOnline     bool
}

// UserRepository defines persistence operations for users.
type UserRepository interface {
	Create(ctx context.Context, user UserRecord) error
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
		`INSERT INTO users (user_id, system_info, registered_at, last_seen, is_online) VALUES (?, ?, ?, ?, ?)`,
		user.UserID,
		user.SystemInfo,
		user.RegisteredAt,
		user.LastSeen,
		boolToInt(user.IsOnline),
	)
	if err != nil {
		if sqliteIsConstraintErr(err) {
			return ErrUserExists
		}
		return err
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
