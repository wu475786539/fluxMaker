package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type Session struct {
	UserID               int64     `json:"user_id"`
	Email                string    `json:"email"`
	Permissions          []string  `json:"permissions"`
	AllInstruments       bool      `json:"all_instruments"`
	Instruments          []string  `json:"instruments"`
	AuthorizationVersion int64     `json:"authorization_version"`
	ExpiresAt            time.Time `json:"expires_at"`
}

func (s Session) Has(permission string) bool {
	for _, value := range s.Permissions {
		if value == permission {
			return true
		}
	}
	return false
}

func (s Session) CanAccessInstrument(instrumentID string) bool {
	if s.AllInstruments {
		return true
	}
	for _, value := range s.Instruments {
		if value == instrumentID {
			return true
		}
	}
	return false
}

type Service struct {
	db         *pgxpool.Pool
	redis      *redis.Client
	sessionTTL time.Duration
}

func NewService(db *pgxpool.Pool, redisClient *redis.Client) *Service {
	return &Service{db: db, redis: redisClient, sessionTTL: 12 * time.Hour}
}

func (s *Service) BootstrapAdmin(ctx context.Context, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD are required")
	}
	var userID int64
	var storedHash string
	var enabled bool
	err := s.db.QueryRow(ctx, `SELECT id,password_hash,enabled FROM users WHERE lower(email)=lower($1)`, email).Scan(&userID, &storedHash, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		hash, err := HashPassword(password)
		if err != nil {
			return err
		}
		if err := s.db.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES($1,$2) RETURNING id`, email, hash).Scan(&userID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !VerifyPassword(storedHash, password) {
		hash, err := HashPassword(password)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(ctx, `UPDATE users SET password_hash=$1,enabled=TRUE,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=$2`, hash, userID); err != nil {
			return err
		}
	} else if !enabled {
		if _, err := s.db.Exec(ctx, `UPDATE users SET enabled=TRUE,authorization_version=authorization_version+1,updated_at=now() WHERE id=$1`, userID); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(ctx, `INSERT INTO user_roles(user_id,role_id) SELECT $1,id FROM roles WHERE code='super_admin' ON CONFLICT DO NOTHING`, userID)
	return err
}

func (s *Service) Login(ctx context.Context, email, password string) (string, Session, error) {
	var userID int64
	var storedHash string
	var enabled bool
	var canonicalEmail string
	err := s.db.QueryRow(ctx, `SELECT id,email,password_hash,enabled FROM users WHERE lower(email)=lower($1)`, strings.TrimSpace(email)).Scan(&userID, &canonicalEmail, &storedHash, &enabled)
	if err != nil || !enabled || !VerifyPassword(storedHash, password) {
		return "", Session{}, ErrInvalidCredentials
	}
	session, err := s.buildSession(ctx, userID, canonicalEmail)
	if err != nil {
		return "", Session{}, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, userID); err != nil {
		return "", Session{}, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", Session{}, err
	}
	token := hex.EncodeToString(tokenBytes)
	payload, _ := json.Marshal(session)
	if err := s.redis.Set(ctx, sessionKey(token), payload, s.sessionTTL).Err(); err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrInvalidCredentials
	}
	payload, err := s.redis.Get(ctx, sessionKey(token)).Bytes()
	if err != nil {
		return Session{}, ErrInvalidCredentials
	}
	var session Session
	if json.Unmarshal(payload, &session) != nil || time.Now().After(session.ExpiresAt) {
		return Session{}, ErrInvalidCredentials
	}
	var enabled bool
	var authorizationVersion int64
	if err := s.db.QueryRow(ctx, `SELECT enabled,authorization_version FROM users WHERE id=$1`, session.UserID).Scan(&enabled, &authorizationVersion); err != nil || !enabled || authorizationVersion != session.AuthorizationVersion {
		_ = s.redis.Del(ctx, sessionKey(token)).Err()
		return Session{}, ErrInvalidCredentials
	}
	return session, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.redis.Del(ctx, sessionKey(token)).Err()
}

func (s *Service) ChangePassword(ctx context.Context, userID int64, currentPassword, newPassword string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var storedHash string
	if err := tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1 AND enabled=TRUE FOR UPDATE`, userID).Scan(&storedHash); err != nil || !VerifyPassword(storedHash, currentPassword) {
		return ErrInvalidCredentials
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$1,authorization_version=authorization_version+1,password_changed_at=now(),updated_at=now() WHERE id=$2`, newHash, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) buildSession(ctx context.Context, userID int64, email string) (Session, error) {
	permissions, err := queryStrings(ctx, s.db, `SELECT DISTINCT rp.permission_code FROM user_roles ur JOIN role_permissions rp ON rp.role_id=ur.role_id WHERE ur.user_id=$1 ORDER BY rp.permission_code`, userID)
	if err != nil {
		return Session{}, err
	}
	instruments, err := queryStrings(ctx, s.db, `SELECT DISTINCT ri.instrument_id FROM user_roles ur JOIN role_instruments ri ON ri.role_id=ur.role_id WHERE ur.user_id=$1 ORDER BY ri.instrument_id`, userID)
	if err != nil {
		return Session{}, err
	}
	var all bool
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(bool_or(r.all_instruments),FALSE) FROM user_roles ur JOIN roles r ON r.id=ur.role_id WHERE ur.user_id=$1`, userID).Scan(&all); err != nil {
		return Session{}, err
	}
	var authorizationVersion int64
	if err := s.db.QueryRow(ctx, `SELECT authorization_version FROM users WHERE id=$1`, userID).Scan(&authorizationVersion); err != nil {
		return Session{}, err
	}
	return Session{UserID: userID, Email: email, Permissions: permissions, AllInstruments: all, Instruments: instruments, AuthorizationVersion: authorizationVersion, ExpiresAt: time.Now().Add(s.sessionTTL)}, nil
}

type stringQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryStrings(ctx context.Context, db stringQueryer, query string, args ...any) ([]string, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "fluxmaker:session:" + hex.EncodeToString(sum[:])
}
