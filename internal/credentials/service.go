package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"fluxmaker/internal/venue"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Metadata struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	VenueType   string    `json:"venue_type"`
	APIKeyLast4 string    `json:"api_key_last4"`
	Fingerprint string    `json:"fingerprint"`
	Enabled     bool      `json:"enabled"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Secret struct {
	APIKey    string
	APISecret string
}

type Service struct {
	db   *pgxpool.Pool
	aead cipher.AEAD
}

func NewService(db *pgxpool.Pool, encodedMasterKey string) (*Service, error) {
	key, err := decodeMasterKey(encodedMasterKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, aead: aead}, nil
}

func (s *Service) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.db.Query(ctx, `SELECT id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at FROM venue_credentials ORDER BY venue_type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Metadata, 0)
	for rows.Next() {
		var item Metadata
		if err := rows.Scan(&item.ID, &item.Name, &item.VenueType, &item.APIKeyLast4, &item.Fingerprint, &item.Enabled, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) Create(ctx context.Context, name, venueType, apiKey, apiSecret string, userID int64) (Metadata, error) {
	name = strings.TrimSpace(name)
	venueType = strings.ToLower(strings.TrimSpace(venueType))
	apiKey = strings.TrimSpace(apiKey)
	if name == "" || !validVenue(venueType) || apiKey == "" || apiSecret == "" {
		return Metadata{}, fmt.Errorf("name, supported venue, api key and secret are required")
	}
	keyCipher, err := s.encrypt(apiKey, venueType+":api-key")
	if err != nil {
		return Metadata{}, err
	}
	secretCipher, err := s.encrypt(apiSecret, venueType+":api-secret")
	if err != nil {
		return Metadata{}, err
	}
	var item Metadata
	err = s.db.QueryRow(ctx, `INSERT INTO venue_credentials(name,venue_type,api_key_cipher,api_secret_cipher,api_key_last4,fingerprint,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$7) RETURNING id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at`, name, venueType, keyCipher, secretCipher, last4(apiKey), fingerprint(apiKey), nullableUser(userID)).Scan(&item.ID, &item.Name, &item.VenueType, &item.APIKeyLast4, &item.Fingerprint, &item.Enabled, &item.UpdatedAt)
	return item, err
}

func (s *Service) Update(ctx context.Context, id int64, name, apiKey, apiSecret string, enabled *bool, userID int64) (Metadata, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Metadata{}, err
	}
	defer tx.Rollback(ctx)
	var venueType string
	var keyCipher, secretCipher []byte
	var currentName, last, fprint string
	var currentEnabled bool
	if err := tx.QueryRow(ctx, `SELECT name,venue_type,api_key_cipher,api_secret_cipher,api_key_last4,fingerprint,enabled FROM venue_credentials WHERE id=$1 FOR UPDATE`, id).Scan(&currentName, &venueType, &keyCipher, &secretCipher, &last, &fprint, &currentEnabled); errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, fmt.Errorf("credential not found")
	} else if err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = currentName
	}
	if strings.TrimSpace(apiKey) != "" {
		keyCipher, err = s.encrypt(strings.TrimSpace(apiKey), venueType+":api-key")
		if err != nil {
			return Metadata{}, err
		}
		last = last4(strings.TrimSpace(apiKey))
		fprint = fingerprint(strings.TrimSpace(apiKey))
	}
	if apiSecret != "" {
		secretCipher, err = s.encrypt(apiSecret, venueType+":api-secret")
		if err != nil {
			return Metadata{}, err
		}
	}
	if enabled != nil {
		currentEnabled = *enabled
	}
	var item Metadata
	if err := tx.QueryRow(ctx, `UPDATE venue_credentials SET name=$1,api_key_cipher=$2,api_secret_cipher=$3,api_key_last4=$4,fingerprint=$5,enabled=$6,updated_by=$7,updated_at=now() WHERE id=$8 RETURNING id,name,venue_type,api_key_last4,fingerprint,enabled,updated_at`, strings.TrimSpace(name), keyCipher, secretCipher, last, fprint, currentEnabled, nullableUser(userID), id).Scan(&item.ID, &item.Name, &item.VenueType, &item.APIKeyLast4, &item.Fingerprint, &item.Enabled, &item.UpdatedAt); err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, err
	}
	return item, nil
}

func (s *Service) Resolve(ctx context.Context, id int64, expectedVenue string) (Secret, error) {
	expectedVenue = strings.ToLower(strings.TrimSpace(expectedVenue))
	var venue string
	var keyCipher, secretCipher []byte
	var enabled bool
	err := s.db.QueryRow(ctx, `SELECT venue_type,api_key_cipher,api_secret_cipher,enabled FROM venue_credentials WHERE id=$1`, id).Scan(&venue, &keyCipher, &secretCipher, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Secret{}, fmt.Errorf("credential %d not found", id)
	} else if err != nil {
		return Secret{}, err
	}
	if !enabled {
		return Secret{}, fmt.Errorf("credential %d is disabled", id)
	}
	if venue != expectedVenue {
		return Secret{}, fmt.Errorf("credential %d belongs to %s, expected %s", id, venue, expectedVenue)
	}
	apiKey, err := s.decrypt(keyCipher, venue+":api-key")
	if err != nil {
		return Secret{}, err
	}
	secret, err := s.decrypt(secretCipher, venue+":api-secret")
	if err != nil {
		return Secret{}, err
	}
	return Secret{APIKey: apiKey, APISecret: secret}, nil
}

func (s *Service) ValidateReference(ctx context.Context, id int64, expectedVenue string) error {
	expectedVenue = strings.ToLower(strings.TrimSpace(expectedVenue))
	var venue string
	var enabled bool
	err := s.db.QueryRow(ctx, `SELECT venue_type,enabled FROM venue_credentials WHERE id=$1`, id).Scan(&venue, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("credential %d not found", id)
	}
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("credential %d is disabled", id)
	}
	if venue != expectedVenue {
		return fmt.Errorf("credential %d belongs to %s, expected %s", id, venue, expectedVenue)
	}
	return nil
}

func (s *Service) encrypt(value, aad string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(value), []byte(aad)), nil
}

func (s *Service) decrypt(value []byte, aad string) (string, error) {
	if len(value) < s.aead.NonceSize() {
		return "", fmt.Errorf("invalid encrypted credential")
	}
	nonce, ciphertext := value[:s.aead.NonceSize()], value[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return string(plain), nil
}

func decodeMasterKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("CREDENTIAL_MASTER_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("CREDENTIAL_MASTER_KEY must be base64 for exactly 32 bytes")
	}
	return key, nil
}
func validVenue(value string) bool {
	_, ok := venue.AdapterSpecFor(value)
	return ok
}
func last4(value string) string {
	r := []rune(value)
	if len(r) <= 4 {
		return string(r)
	}
	return string(r[len(r)-4:])
}
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}
func nullableUser(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}
