package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrFileNotFound    = errors.New("file not found")
	ErrFileForbidden   = errors.New("forbidden access to file")
	ErrFileTooLarge    = errors.New("file exceeds maximum allowed size")
	ErrStorageQuota    = errors.New("storage quota exceeded")
	ErrInvalidFileType = errors.New("unsupported or invalid file type")
	ErrInvalidFilename = errors.New("invalid filename")
	ErrFileInUse       = errors.New("file is currently in use by an active task")
)

// FileRecord mirrors OpenAI File metadata plus ownership & quota state.
type FileRecord struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Bytes       int64  `json:"bytes"`
	CreatedAt   int64  `json:"created_at"`
	Filename    string `json:"filename"`
	Purpose     string `json:"purpose"`
	OwnerID     string `json:"-"`
	ContentType string `json:"-"`
	SHA256      string `json:"-"`
	StorageName string `json:"-"`
	ExpiresAt   int64  `json:"-"`
	ActiveRefs  int    `json:"-"`
}

// FileStore manages safe, owner-isolated local file storage and SQLite metadata (Section 6.2-6.5, 11.2).
type FileStore struct {
	db           *DB
	rootDir      string
	filesDir     string
	maxFileBytes int64
	quotaBytes   int64
	fileTTL      time.Duration
}

// NewFileStore ensures managed directories exist and rejects symlinked root storage directories.
func NewFileStore(db *DB, rootDir string, maxFileBytes, quotaBytes int64, fileTTL time.Duration) (*FileStore, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := ensureNoSymlink(absRoot); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	filesDir := filepath.Join(absRoot, "files")
	if err := ensureNoSymlink(filesDir); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}
	if err := ensureNoSymlink(filesDir); err != nil {
		return nil, err
	}
	return &FileStore{
		db:           db,
		rootDir:      absRoot,
		filesDir:     filesDir,
		maxFileBytes: maxFileBytes,
		quotaBytes:   quotaBytes,
		fileTTL:      fileTTL,
	}, nil
}

// RootDir returns the absolute managed storage path.
func (s *FileStore) RootDir() string {
	return s.rootDir
}

// CheckWritable verifies the managed files directory can create and delete a probe file.
func (s *FileStore) CheckWritable() error {
	f, err := os.CreateTemp(s.filesDir, ".ready-*")
	if err != nil {
		return fmt.Errorf("storage not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// Save streams an uploaded file safely into a temporary file, validates size, quota, and type,
// and commits metadata to SQLite.
func (s *FileStore) Save(ctx context.Context, ownerID, rawFilename, purpose string, r io.Reader) (FileRecord, error) {
	if purpose != "user_data" {
		return FileRecord{}, fmt.Errorf("%w: purpose must be user_data", ErrInvalidFileType)
	}

	safeName, err := SanitizeFilename(rawFilename)
	if err != nil {
		return FileRecord{}, err
	}

	// Check current storage usage against quota before accepting large uploads.
	usedBytes, err := s.TotalStoredBytes(ctx)
	if err != nil {
		return FileRecord{}, err
	}
	if usedBytes >= s.quotaBytes {
		return FileRecord{}, ErrStorageQuota
	}

	tmpFile, err := os.CreateTemp(s.filesDir, ".upload-*")
	if err != nil {
		return FileRecord{}, fmt.Errorf("create temp upload file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		_ = tmpFile.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	sniffBuf := make([]byte, 0, 8192)
	buf := make([]byte, 32*1024)
	var total int64

	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > s.maxFileBytes {
				return FileRecord{}, ErrFileTooLarge
			}
			if usedBytes+total > s.quotaBytes {
				return FileRecord{}, ErrStorageQuota
			}
			chunk := buf[:n]
			if bytes.IndexByte(chunk, 0) >= 0 {
				return FileRecord{}, fmt.Errorf("%w: binary null bytes are not permitted", ErrInvalidFileType)
			}
			if len(sniffBuf) < 8192 {
				need := 8192 - len(sniffBuf)
				if n < need {
					need = n
				}
				sniffBuf = append(sniffBuf, chunk[:need]...)
			}
			if _, err := tmpFile.Write(chunk); err != nil {
				return FileRecord{}, fmt.Errorf("write temp file: %w", err)
			}
			_, _ = hasher.Write(chunk)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return FileRecord{}, fmt.Errorf("read upload: %w", readErr)
		}
	}

	if total == 0 {
		return FileRecord{}, fmt.Errorf("%w: file must not be empty", ErrInvalidFileType)
	}

	contentType := detectAllowedTextType(safeName, sniffBuf)
	if contentType == "" {
		return FileRecord{}, fmt.Errorf("%w: only UTF-8 text and markdown files are allowed", ErrInvalidFileType)
	}

	// Validate full UTF-8 integrity for non-trivial files up to maxFileBytes.
	if err := validateFileUTF8(tmpFile); err != nil {
		return FileRecord{}, fmt.Errorf("%w: %v", ErrInvalidFileType, err)
	}

	if err := tmpFile.Chmod(0o600); err != nil {
		return FileRecord{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return FileRecord{}, err
	}

	fileID := newID("file")
	destPath := filepath.Join(s.filesDir, fileID)
	if err := os.Rename(tmpPath, destPath); err != nil {
		return FileRecord{}, fmt.Errorf("finalize file: %w", err)
	}
	cleanupTmp = false

	now := time.Now().Unix()
	expiresAt := now + int64(s.fileTTL.Seconds())
	sumHex := hex.EncodeToString(hasher.Sum(nil))

	rec := FileRecord{
		ID:          fileID,
		Object:      "file",
		Bytes:       total,
		CreatedAt:   now,
		Filename:    safeName,
		Purpose:     purpose,
		OwnerID:     ownerID,
		ContentType: contentType,
		SHA256:      sumHex,
		StorageName: fileID,
		ExpiresAt:   expiresAt,
	}

	if _, err := s.db.SQL().ExecContext(ctx, `
		INSERT INTO files (id, owner_id, filename, purpose, bytes, content_type, sha256, storage_name, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.ID, rec.OwnerID, rec.Filename, rec.Purpose, rec.Bytes, rec.ContentType, rec.SHA256, rec.StorageName, rec.CreatedAt, rec.ExpiresAt); err != nil {
		_ = os.Remove(destPath)
		return FileRecord{}, fmt.Errorf("insert file metadata: %w", err)
	}

	return rec, nil
}

// Get checks existence and ownership of a file_id.
func (s *FileStore) Get(ctx context.Context, ownerID, fileID string) (FileRecord, error) {
	if !isSafeID(fileID, "file_") {
		return FileRecord{}, ErrFileNotFound
	}
	var rec FileRecord
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, filename, purpose, bytes, content_type, sha256, storage_name, created_at, expires_at, active_refs
		FROM files
		WHERE id = ? AND deleted = 0
	`, fileID).Scan(
		&rec.ID, &rec.OwnerID, &rec.Filename, &rec.Purpose, &rec.Bytes,
		&rec.ContentType, &rec.SHA256, &rec.StorageName, &rec.CreatedAt, &rec.ExpiresAt, &rec.ActiveRefs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileRecord{}, ErrFileNotFound
	}
	if err != nil {
		return FileRecord{}, err
	}
	rec.Object = "file"
	if rec.OwnerID != ownerID {
		return FileRecord{}, ErrFileForbidden
	}
	if rec.ExpiresAt > 0 && time.Now().Unix() > rec.ExpiresAt && rec.ActiveRefs == 0 {
		return FileRecord{}, ErrFileNotFound
	}
	return rec, nil
}

// OpenContent opens the verified file for reading strictly inside filesDir, rejecting symlinks.
func (s *FileStore) OpenContent(ctx context.Context, ownerID, fileID string) (FileRecord, *os.File, error) {
	rec, err := s.Get(ctx, ownerID, fileID)
	if err != nil {
		return FileRecord{}, nil, err
	}
	path, err := s.safeStoragePath(rec.StorageName)
	if err != nil {
		return FileRecord{}, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileRecord{}, nil, ErrFileNotFound
		}
		return FileRecord{}, nil, err
	}
	return rec, f, nil
}

// Delete removes a file if owned by ownerID and not referenced by an active task (409 Conflict).
func (s *FileStore) Delete(ctx context.Context, ownerID, fileID string) error {
	rec, err := s.Get(ctx, ownerID, fileID)
	if err != nil {
		return err
	}

	res, err := s.db.SQL().ExecContext(ctx, `
		UPDATE files
		SET deleted = 1
		WHERE id = ? AND owner_id = ? AND deleted = 0 AND active_refs = 0
	`, rec.ID, ownerID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Re-check whether file is active.
		var activeRefs int
		err := s.db.SQL().QueryRowContext(ctx, `SELECT active_refs FROM files WHERE id = ? AND deleted = 0`, rec.ID).Scan(&activeRefs)
		if err == nil && activeRefs > 0 {
			return ErrFileInUse
		}
		return ErrFileNotFound
	}

	if path, err := s.safeStoragePath(rec.StorageName); err == nil {
		_ = os.Remove(path)
	}
	return nil
}

// AcquireForRun increments active_refs for all validated file IDs owned by ownerID.
func (s *FileStore) AcquireForRun(ctx context.Context, ownerID string, fileIDs []string) ([]FileRecord, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	records := make([]FileRecord, 0, len(fileIDs))
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	for _, fid := range fileIDs {
		if !isSafeID(fid, "file_") {
			return nil, ErrFileNotFound
		}
		var rec FileRecord
		err := tx.QueryRowContext(ctx, `
			SELECT id, owner_id, filename, purpose, bytes, content_type, sha256, storage_name, created_at, expires_at, active_refs
			FROM files WHERE id = ? AND deleted = 0
		`, fid).Scan(
			&rec.ID, &rec.OwnerID, &rec.Filename, &rec.Purpose, &rec.Bytes,
			&rec.ContentType, &rec.SHA256, &rec.StorageName, &rec.CreatedAt, &rec.ExpiresAt, &rec.ActiveRefs,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		if err != nil {
			return nil, err
		}
		if rec.OwnerID != ownerID {
			return nil, ErrFileForbidden
		}
		if rec.ExpiresAt > 0 && now > rec.ExpiresAt {
			return nil, ErrFileNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET active_refs = active_refs + 1 WHERE id = ?`, fid); err != nil {
			return nil, err
		}
		rec.Object = "file"
		records = append(records, rec)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return records, nil
}

// ReleaseForRun decrements active_refs once a run finishes.
func (s *FileStore) ReleaseForRun(ctx context.Context, fileIDs []string) {
	for _, fid := range fileIDs {
		_, _ = s.db.SQL().ExecContext(ctx, `
			UPDATE files SET active_refs = CASE WHEN active_refs > 0 THEN active_refs - 1 ELSE 0 END WHERE id = ?
		`, fid)
	}
}

// CopyToWorkspace copies an authorized uploaded file into destPath safely without symlinks.
func (s *FileStore) CopyToWorkspace(rec FileRecord, destPath string) error {
	srcPath, err := s.safeStoragePath(rec.StorageName)
	if err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// ListAll returns non-deleted file records across all owners for Admin inspection.
func (s *FileStore) ListAll(ctx context.Context, limit int) ([]FileRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, owner_id, filename, purpose, bytes, content_type, sha256, storage_name, created_at, expires_at, active_refs
		FROM files
		WHERE deleted = 0
		ORDER BY created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := rows.Scan(
			&rec.ID, &rec.OwnerID, &rec.Filename, &rec.Purpose, &rec.Bytes,
			&rec.ContentType, &rec.SHA256, &rec.StorageName, &rec.CreatedAt, &rec.ExpiresAt, &rec.ActiveRefs,
		); err != nil {
			return nil, err
		}
		rec.Object = "file"
		out = append(out, rec)
	}
	return out, rows.Err()
}

// OpenContentByID opens any non-deleted file by ID for Admin inspection.
func (s *FileStore) OpenContentByID(ctx context.Context, fileID string) (FileRecord, *os.File, error) {
	if !isSafeID(fileID, "file_") {
		return FileRecord{}, nil, ErrFileNotFound
	}
	var rec FileRecord
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, owner_id, filename, purpose, bytes, content_type, sha256, storage_name, created_at, expires_at, active_refs
		FROM files WHERE id = ? AND deleted = 0
	`, fileID).Scan(
		&rec.ID, &rec.OwnerID, &rec.Filename, &rec.Purpose, &rec.Bytes,
		&rec.ContentType, &rec.SHA256, &rec.StorageName, &rec.CreatedAt, &rec.ExpiresAt, &rec.ActiveRefs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return FileRecord{}, nil, ErrFileNotFound
	}
	if err != nil {
		return FileRecord{}, nil, err
	}
	rec.Object = "file"
	path, err := s.safeStoragePath(rec.StorageName)
	if err != nil {
		return FileRecord{}, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileRecord{}, nil, ErrFileNotFound
		}
		return FileRecord{}, nil, err
	}
	return rec, f, nil
}

// DeleteByID removes an idle file regardless of owner for Admin maintenance.
func (s *FileStore) DeleteByID(ctx context.Context, fileID string) error {
	if !isSafeID(fileID, "file_") {
		return ErrFileNotFound
	}
	var storageName string
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT storage_name FROM files WHERE id = ? AND deleted = 0
	`, fileID).Scan(&storageName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFileNotFound
	}
	if err != nil {
		return err
	}
	res, err := s.db.SQL().ExecContext(ctx, `
		UPDATE files SET deleted = 1 WHERE id = ? AND deleted = 0 AND active_refs = 0
	`, fileID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFileInUse
	}
	if path, err := s.safeStoragePath(storageName); err == nil {
		_ = os.Remove(path)
	}
	return nil
}

// TotalStoredBytes returns the total byte size of non-deleted stored files.
func (s *FileStore) TotalStoredBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM files WHERE deleted = 0`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// CleanupExpired removes expired files that are not currently active.
func (s *FileStore) CleanupExpired(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, storage_name FROM files
		WHERE deleted = 0 AND active_refs = 0 AND expires_at <= ?
	`, now)
	if err != nil {
		return 0, err
	}
	type item struct {
		id          string
		storageName string
	}
	var toDelete []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.storageName); err == nil {
			toDelete = append(toDelete, it)
		}
	}
	_ = rows.Close()

	removed := 0
	for _, it := range toDelete {
		res, err := s.db.SQL().ExecContext(ctx, `UPDATE files SET deleted = 1 WHERE id = ? AND active_refs = 0`, it.id)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if path, err := s.safeStoragePath(it.storageName); err == nil {
				_ = os.Remove(path)
			}
			removed++
		}
	}
	return removed, nil
}

func (s *FileStore) safeStoragePath(storageName string) (string, error) {
	if storageName == "" || strings.ContainsAny(storageName, `/\`) || strings.Contains(storageName, "..") {
		return "", ErrInvalidFilename
	}
	target := filepath.Join(s.filesDir, storageName)
	rel, err := filepath.Rel(s.filesDir, target)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return "", ErrInvalidFilename
	}
	if err := ensureNoSymlink(target); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return target, nil
}

func ensureNoSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink not permitted: %s", filepath.Base(path))
	}
	return nil
}

// SanitizeFilename validates filenames, rejecting path separators, traversal tokens,
// control characters, leading dots, and overly long names (Section 6.2 & 11.2).
func SanitizeFilename(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsRune(trimmed, 0) || strings.ContainsAny(trimmed, `/\:`) || strings.Contains(trimmed, "..") {
		return "", ErrInvalidFilename
	}
	if trimmed == "." || strings.HasPrefix(trimmed, ".") {
		return "", ErrInvalidFilename
	}
	runeCount := 0
	for _, r := range trimmed {
		if r < 32 || r == 127 {
			return "", ErrInvalidFilename
		}
		runeCount++
	}
	if runeCount > 128 {
		return "", ErrInvalidFilename
	}
	return trimmed, nil
}

func detectAllowedTextType(filename string, sample []byte) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md", ".markdown", ".txt", ".json", ".yaml", ".yml", ".csv", ".tsv", ".xml", ".log", ".prompt", ".go", ".py", ".js", ".ts", ".sql", ".sh", ".html", ".css":
	case "":
		// Allow extensionless text files if content sniffs as text.
	default:
		// Reject known dangerous/binary extensions outright.
		switch ext {
		case ".exe", ".dll", ".so", ".dylib", ".bin", ".elf", ".shx", ".zip", ".tar", ".gz", ".png", ".jpg", ".jpeg", ".pdf":
			return ""
		}
	}

	ct := http.DetectContentType(sample)
	if strings.HasPrefix(ct, "text/") || ct == "application/json" || ct == "application/xml" {
		return ct
	}
	// http.DetectContentType may return application/octet-stream for non-ASCII UTF-8 lead bytes on small samples;
	// trim at most 3 trailing incomplete rune bytes if sample filled the 8KiB buffer, then check UTF-8 validity.
	check := sample
	if len(check) == 8192 {
		for i := 1; i <= 3 && i <= len(check); i++ {
			if utf8.RuneStart(check[len(check)-i]) {
				r, size := utf8.DecodeRune(check[len(check)-i:])
				if r == utf8.RuneError && size == 1 {
					check = check[:len(check)-i]
				}
				break
			}
		}
	}
	if utf8.Valid(check) {
		return "text/plain; charset=utf-8"
	}
	return ""
}

func validateFileUTF8(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := append(carry, buf[:n]...)
			carry = carry[:0]
			// Trim up to 3 trailing incomplete UTF-8 rune bytes at chunk boundary.
			validLen := len(chunk)
			for i := 1; i <= 3 && i <= validLen; i++ {
				if utf8.RuneStart(chunk[validLen-i]) {
					r, size := utf8.DecodeRune(chunk[validLen-i:])
					if r == utf8.RuneError && size == 1 && err != io.EOF {
						carry = append(carry, chunk[validLen-i:]...)
						validLen -= i
					}
					break
				}
			}
			if !utf8.Valid(chunk[:validLen]) {
				return errors.New("file content is not valid UTF-8")
			}
		}
		if err == io.EOF {
			if len(carry) > 0 && !utf8.Valid(carry) {
				return errors.New("trailing bytes are not valid UTF-8")
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func isSafeID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 64 {
		return false
	}
	for _, r := range id[len(prefix):] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
