package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"fluxmaker/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const activeCacheKey = "fluxmaker:config:active"

var (
	ErrNoDraft  = errors.New("no draft configuration")
	ErrNoActive = errors.New("no published configuration")
)

type Snapshot struct {
	Version     int64         `json:"version"`
	Config      config.Config `json:"config"`
	PublishedAt time.Time     `json:"published_at"`
}

type Store struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

func New(db *pgxpool.Pool, redisClient *redis.Client) *Store {
	return &Store{db: db, redis: redisClient}
}

func (s *Store) GetDraft(ctx context.Context) (config.Config, error) {
	var payload []byte
	err := s.db.QueryRow(ctx, `SELECT payload FROM draft_configs WHERE id=1`).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return config.Config{}, ErrNoDraft
	}
	if err != nil {
		return config.Config{}, err
	}
	var cfg config.Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func (s *Store) PutDraft(ctx context.Context, cfg config.Config, userID int64) error {
	cfg.NormalizeStrategySizing()
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO draft_configs(id,payload,updated_by,updated_at) VALUES(1,$1,$2,now()) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_by=excluded.updated_by,updated_at=now()`, payload, nullableUser(userID))
	if err == nil {
		_ = s.audit(ctx, userID, "config.draft.update", "config", "draft", map[string]any{})
	}
	return err
}

func (s *Store) PublishDraft(ctx context.Context, userID int64) (Snapshot, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(764223)`); err != nil {
		return Snapshot{}, err
	}
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM draft_configs WHERE id=1 FOR UPDATE`).Scan(&payload); errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNoDraft
	} else if err != nil {
		return Snapshot{}, err
	}
	var cfg config.Config
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return Snapshot{}, err
	}
	cfg.NormalizeStrategySizing()
	if err := cfg.Validate(); err != nil {
		return Snapshot{}, err
	}
	payload, err = json.Marshal(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM config_snapshots`).Scan(&version); err != nil {
		return Snapshot{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE config_snapshots SET active=FALSE WHERE active`); err != nil {
		return Snapshot{}, err
	}
	var publishedAt time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO config_snapshots(version,payload,active,published_by) VALUES($1,$2,TRUE,$3) RETURNING published_at`, version, payload, nullableUser(userID)).Scan(&publishedAt); err != nil {
		return Snapshot{}, err
	}
	details, _ := json.Marshal(map[string]any{"version": version})
	if _, err := tx.Exec(ctx, `INSERT INTO audit_logs(user_id,action,resource_type,resource_id,details) VALUES($1,'config.publish','config',$2,$3)`, nullableUser(userID), fmt.Sprint(version), details); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Version: version, Config: cfg, PublishedAt: publishedAt}
	// PostgreSQL is the source of truth. Cache refresh is best effort after the
	// transaction has committed; LoadActive always checks the database version
	// before trusting cached payloads.
	_ = s.cache(ctx, snapshot)
	return snapshot, nil
}

func (s *Store) LoadActive(ctx context.Context) (Snapshot, error) {
	if s.db == nil {
		if cached, ok := s.cachedActive(ctx); ok {
			return cached, nil
		}
		return Snapshot{}, ErrNoActive
	}
	var activeVersion int64
	var publishedAt time.Time
	err := s.db.QueryRow(ctx, `SELECT version,published_at FROM config_snapshots WHERE active=TRUE ORDER BY version DESC LIMIT 1`).Scan(&activeVersion, &publishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNoActive
	}
	if err != nil {
		// A database outage cannot introduce a newer committed configuration,
		// so a previously validated cache remains a safe availability fallback.
		if cached, ok := s.cachedActive(ctx); ok {
			return cached, nil
		}
		return Snapshot{}, err
	}
	if cached, ok := s.cachedActive(ctx); ok && cached.Version == activeVersion {
		return cached, nil
	}

	snapshot := Snapshot{Version: activeVersion, PublishedAt: publishedAt}
	var payload []byte
	err = s.db.QueryRow(ctx, `SELECT payload FROM config_snapshots WHERE version=$1`, activeVersion).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNoActive
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal(payload, &snapshot.Config); err != nil {
		return Snapshot{}, err
	}
	if err := snapshot.Config.Validate(); err != nil {
		return Snapshot{}, err
	}
	_ = s.cache(ctx, snapshot)
	return snapshot, nil
}

func (s *Store) LoadVersion(ctx context.Context, version int64) (Snapshot, error) {
	var snapshot Snapshot
	var payload []byte
	err := s.db.QueryRow(ctx, `SELECT version,payload,published_at FROM config_snapshots WHERE version=$1`, version).Scan(&snapshot.Version, &payload, &snapshot.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNoActive
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal(payload, &snapshot.Config); err != nil {
		return Snapshot{}, err
	}
	if err := snapshot.Config.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) cachedActive(ctx context.Context) (Snapshot, bool) {
	if s.redis == nil {
		return Snapshot{}, false
	}
	raw, err := s.redis.Get(ctx, activeCacheKey).Bytes()
	if err != nil {
		return Snapshot{}, false
	}
	var snapshot Snapshot
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Version <= 0 || snapshot.Config.Validate() != nil {
		return Snapshot{}, false
	}
	return snapshot, true
}

func (s *Store) cache(ctx context.Context, snapshot Snapshot) error {
	if s.redis == nil {
		return nil
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return s.redis.Set(ctx, activeCacheKey, b, 24*time.Hour).Err()
}

func (s *Store) audit(ctx context.Context, userID int64, action, resourceType, resourceID string, details any) error {
	b, _ := json.Marshal(details)
	_, err := s.db.Exec(ctx, `INSERT INTO audit_logs(user_id,action,resource_type,resource_id,details) VALUES($1,$2,$3,$4,$5)`, nullableUser(userID), action, resourceType, resourceID, b)
	return err
}

func nullableUser(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}
