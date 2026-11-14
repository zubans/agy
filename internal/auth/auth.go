package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/storage"
)

var (
	ErrUnauthorized = errors.New("invalid or missing API key")
	ErrKeyRevoked   = errors.New("API key has been revoked")
)

// Principal represents the authenticated API key and its owner/permissions.
type Principal struct {
	KeyID         string
	OwnerID       string
	Name          string
	AllowedModels []string
	MaxConcurrent int
	Revoked       bool
}

// AllowsModel checks whether this API key is permitted to invoke modelID (Section 11.1).
func (p Principal) AllowsModel(modelID string) bool {
	for _, allowed := range p.AllowedModels {
		if allowed == "*" || allowed == modelID {
			return true
		}
	}
	return false
}

// Store manages API key synchronization and constant-time bearer verification backed by SQLite.
type Store struct {
	db *storage.DB
}

// NewStore creates an auth store and synchronizes configured API keys (hashed) into SQLite.
func NewStore(ctx context.Context, db *storage.DB, keys []config.APIKeyConfig) (*Store, error) {
	s := &Store{db: db}
	if err := s.SyncConfigKeys(ctx, keys); err != nil {
		return nil, err
	}
	return s, nil
}

// SyncConfigKeys upserts configured API keys by SHA-256 hash so plaintext keys are never persisted or logged.
func (s *Store) SyncConfigKeys(ctx context.Context, keys []config.APIKeyConfig) error {
	now := time.Now().Unix()
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, k := range keys {
		allowedJSON, err := json.Marshal(k.AllowedModels)
		if err != nil {
			return err
		}
		revokedInt := 0
		var revokedAt any
		if k.Revoked {
			revokedInt = 1
			revokedAt = now
		}
		hash := k.EffectiveHash()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO api_keys (id, owner_id, name, key_hash, allowed_models_json, max_concurrent, revoked, created_at, revoked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				owner_id = excluded.owner_id,
				name = excluded.name,
				key_hash = excluded.key_hash,
				allowed_models_json = excluded.allowed_models_json,
				max_concurrent = excluded.max_concurrent,
				revoked = excluded.revoked,
				revoked_at = COALESCE(excluded.revoked_at, api_keys.revoked_at)
		`, k.ID, k.OwnerID, k.Name, hash, string(allowedJSON), k.MaxConcurrent, revokedInt, now, revokedAt); err != nil {
			return fmt.Errorf("upsert api key %q: %w", k.ID, err)
		}
	}
	return tx.Commit()
}

// AuthenticateBearer validates an Authorization header value ("Bearer <token>").
func (s *Store) AuthenticateBearer(ctx context.Context, authHeader string) (Principal, error) {
	token, ok := parseBearerToken(authHeader)
	if !ok {
		return Principal{}, ErrUnauthorized
	}

	sum := sha256.Sum256([]byte(token))
	hashHex := hex.EncodeToString(sum[:])

	var (
		p           Principal
		storedHash  string
		allowedJSON string
		revokedInt  int
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, name, key_hash, allowed_models_json, max_concurrent, revoked
		FROM api_keys
		WHERE key_hash = ?
	`, hashHex).Scan(&p.KeyID, &p.OwnerID, &p.Name, &storedHash, &allowedJSON, &p.MaxConcurrent, &revokedInt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("lookup api key: %w", err)
	}

	if subtle.ConstantTimeCompare([]byte(hashHex), []byte(storedHash)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	if revokedInt != 0 {
		return Principal{}, ErrKeyRevoked
	}
	if err := json.Unmarshal([]byte(allowedJSON), &p.AllowedModels); err != nil {
		p.AllowedModels = nil
	}
	return p, nil
}

// KeySummary exposes safe metadata for an API key without leaking any secret or hash.
type KeySummary struct {
	ID            string   `json:"id"`
	OwnerID       string   `json:"owner_id"`
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	MaxConcurrent int      `json:"max_concurrent"`
	Revoked       bool     `json:"revoked"`
	CreatedAt     int64    `json:"created_at"`
	RevokedAt     *int64   `json:"revoked_at,omitempty"`
}

// ListKeys returns all API keys ordered by creation time descending (metadata only).
func (s *Store) ListKeys(ctx context.Context) ([]KeySummary, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, owner_id, name, allowed_models_json, max_concurrent, revoked, created_at, revoked_at
		FROM api_keys
		ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KeySummary
	for rows.Next() {
		var (
			ks          KeySummary
			allowedJSON string
			revokedInt  int
			revokedAt   sql.NullInt64
		)
		if err := rows.Scan(&ks.ID, &ks.OwnerID, &ks.Name, &allowedJSON, &ks.MaxConcurrent, &revokedInt, &ks.CreatedAt, &revokedAt); err != nil {
			return nil, err
		}
		ks.Revoked = revokedInt != 0
		if revokedAt.Valid {
			v := revokedAt.Int64
			ks.RevokedAt = &v
		}
		if err := json.Unmarshal([]byte(allowedJSON), &ks.AllowedModels); err != nil {
			ks.AllowedModels = []string{}
		}
		out = append(out, ks)
	}
	return out, rows.Err()
}

// CreateKey issues a new random API key, stores only its SHA-256 hash, and returns the secret once.
func (s *Store) CreateKey(ctx context.Context, ownerID, name string, allowedModels []string, maxConcurrent int) (KeySummary, string, error) {
	ownerID = strings.TrimSpace(ownerID)
	name = strings.TrimSpace(name)
	if ownerID == "" {
		return KeySummary{}, "", errors.New("owner_id is required")
	}
	if name == "" {
		name = ownerID
	}
	if len(allowedModels) == 0 {
		allowedModels = []string{"*"}
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	var idRand [8]byte
	var secRand [24]byte
	_, _ = rand.Read(idRand[:])
	_, _ = rand.Read(secRand[:])

	keyID := "key_dyn_" + hex.EncodeToString(idRand[:])
	plaintext := "gw-" + hex.EncodeToString(secRand[:])
	sum := sha256.Sum256([]byte(plaintext))
	hashHex := hex.EncodeToString(sum[:])

	allowedJSON, err := json.Marshal(allowedModels)
	if err != nil {
		return KeySummary{}, "", err
	}
	now := time.Now().Unix()

	if _, err := s.db.SQL().ExecContext(ctx, `
		INSERT INTO api_keys (id, owner_id, name, key_hash, allowed_models_json, max_concurrent, revoked, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)
	`, keyID, ownerID, name, hashHex, string(allowedJSON), maxConcurrent, now); err != nil {
		return KeySummary{}, "", err
	}

	return KeySummary{
		ID:            keyID,
		OwnerID:       ownerID,
		Name:          name,
		AllowedModels: allowedModels,
		MaxConcurrent: maxConcurrent,
		Revoked:       false,
		CreatedAt:     now,
	}, plaintext, nil
}

// UpdateKey modifies mutable metadata on an existing API key.
func (s *Store) UpdateKey(ctx context.Context, keyID string, name *string, allowedModels []string, maxConcurrent *int) (KeySummary, error) {
	var (
		ks          KeySummary
		allowedJSON string
		revokedInt  int
		revokedAt   sql.NullInt64
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, name, allowed_models_json, max_concurrent, revoked, created_at, revoked_at
		FROM api_keys WHERE id = ?
	`, keyID).Scan(&ks.ID, &ks.OwnerID, &ks.Name, &allowedJSON, &ks.MaxConcurrent, &revokedInt, &ks.CreatedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return KeySummary{}, fmt.Errorf("api key %q not found", keyID)
	}
	if err != nil {
		return KeySummary{}, err
	}
	ks.Revoked = revokedInt != 0
	if revokedAt.Valid {
		v := revokedAt.Int64
		ks.RevokedAt = &v
	}
	_ = json.Unmarshal([]byte(allowedJSON), &ks.AllowedModels)

	if name != nil && strings.TrimSpace(*name) != "" {
		ks.Name = strings.TrimSpace(*name)
	}
	if allowedModels != nil {
		if len(allowedModels) == 0 {
			return KeySummary{}, errors.New("allowed_models must not be empty")
		}
		ks.AllowedModels = allowedModels
	}
	if maxConcurrent != nil {
		if *maxConcurrent < 1 || *maxConcurrent > 256 {
			return KeySummary{}, errors.New("max_concurrent must be between 1 and 256")
		}
		ks.MaxConcurrent = *maxConcurrent
	}

	newAllowed, err := json.Marshal(ks.AllowedModels)
	if err != nil {
		return KeySummary{}, err
	}
	if _, err := s.db.SQL().ExecContext(ctx, `
		UPDATE api_keys SET name = ?, allowed_models_json = ?, max_concurrent = ? WHERE id = ?
	`, ks.Name, string(newAllowed), ks.MaxConcurrent, keyID); err != nil {
		return KeySummary{}, err
	}
	return ks, nil
}

// RevokeKey marks an API key as revoked in SQLite.
func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	return s.SetRevoked(ctx, keyID, true)
}

// SetRevoked toggles key revocation state.
func (s *Store) SetRevoked(ctx context.Context, keyID string, revoked bool) error {
	var (
		res sql.Result
		err error
	)
	if revoked {
		now := time.Now().Unix()
		res, err = s.db.SQL().ExecContext(ctx, `
			UPDATE api_keys SET revoked = 1, revoked_at = ? WHERE id = ?
		`, now, keyID)
	} else {
		res, err = s.db.SQL().ExecContext(ctx, `
			UPDATE api_keys SET revoked = 0, revoked_at = NULL WHERE id = ?
		`, keyID)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key %q not found", keyID)
	}
	return nil
}

// DeleteKey permanently removes an API key from SQLite.
func (s *Store) DeleteKey(ctx context.Context, keyID string) error {
	res, err := s.db.SQL().ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key %q not found", keyID)
	}
	return nil
}

func parseBearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}
