package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cli-ai-gateway/internal/storage"
)

func openTestFileStore(t *testing.T, maxFile, quota int64, ttl time.Duration) (*storage.DB, *storage.FileStore, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "gateway.db")
	db, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fs, err := storage.NewFileStore(db, root, maxFile, quota, ttl)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return db, fs, root
}

func TestSanitizeFilename(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"prompt.md", "prompt.md"},
		{"notes-2026_v1.txt", "notes-2026_v1.txt"},
		{"отчёт.md", "отчёт.md"},
		{"file name.json", "file name.json"},
	}
	for _, tc := range valid {
		got, err := storage.SanitizeFilename(tc.in)
		if err != nil {
			t.Fatalf("SanitizeFilename(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		".",
		"..",
		"../etc/passwd",
		`..\secret.txt`,
		".hidden",
		"bad\x00name.txt",
		"ctrl\nname.txt",
		strings.Repeat("a", 200),
	}
	for _, in := range invalid {
		if got, err := storage.SanitizeFilename(in); err == nil {
			t.Fatalf("SanitizeFilename(%q) succeeded with %q, expected error", in, got)
		}
	}
}

func TestNewFileStoreRejectsSymlinkDir(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "linkroot")
	if err := os.Symlink(realDir, linkRoot); err != nil {
		t.Skipf("symlink unsupported in env: %v", err)
	}

	db, err := storage.OpenSQLite(context.Background(), filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := storage.NewFileStore(db, linkRoot, 1024, 2048, time.Hour); err == nil {
		t.Fatal("expected NewFileStore to reject symlinked root directory")
	}
}

func TestFileStoreSaveMultibyteUTF8BoundaryAndValidation(t *testing.T) {
	ctx := context.Background()
	_, fs, _ := openTestFileStore(t, 5*1024*1024, 10*1024*1024, time.Hour)

	// Build payload where a 2-byte UTF-8 rune crosses the 8192-byte sniff boundary.
	prefix := strings.Repeat("a", 8191)
	cyrillic := strings.Repeat("Я", 20000) // multi-byte runes spanning both 8KiB and 32KiB chunks
	content := prefix + cyrillic

	rec, err := fs.Save(ctx, "user-1", "prompt.md", "user_data", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Save multibyte UTF-8 across boundary failed: %v", err)
	}
	if rec.Bytes != int64(len(content)) {
		t.Fatalf("Bytes mismatch: got %d, want %d", rec.Bytes, len(content))
	}

	gotMeta, err := fs.Get(ctx, "user-1", rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotMeta.Filename != "prompt.md" || gotMeta.Purpose != "user_data" {
		t.Fatalf("unexpected metadata: %+v", gotMeta)
	}

	_, rc, err := fs.OpenContent(ctx, "user-1", rec.ID)
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	readBack, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(readBack) != content {
		t.Fatalf("read back content mismatch (len=%d vs %d)", len(readBack), len(content))
	}

	// Binary NUL bytes must be rejected.
	if _, err := fs.Save(ctx, "user-1", "bad.txt", "user_data", bytes.NewReader([]byte("ok\x00bin"))); !errors.Is(err, storage.ErrInvalidFileType) {
		t.Fatalf("expected ErrInvalidFileType for NUL bytes, got %v", err)
	}

	// Wrong purpose must be rejected.
	if _, err := fs.Save(ctx, "user-1", "prompt.md", "fine-tune", strings.NewReader("hi")); !errors.Is(err, storage.ErrInvalidFileType) {
		t.Fatalf("expected ErrInvalidFileType for wrong purpose, got %v", err)
	}

	// Disallowed extension (.exe) must be rejected.
	if _, err := fs.Save(ctx, "user-1", "payload.exe", "user_data", strings.NewReader("echo hi")); !errors.Is(err, storage.ErrInvalidFileType) {
		t.Fatalf("expected ErrInvalidFileType for .exe, got %v", err)
	}

	// Invalid UTF-8 after 8192 sniff window must still be caught by full-stream check.
	invalidLate := strings.Repeat("x", 9000) + string([]byte{0xff, 0xfe, 0xfd})
	if _, err := fs.Save(ctx, "user-1", "late.md", "user_data", strings.NewReader(invalidLate)); !errors.Is(err, storage.ErrInvalidFileType) {
		t.Fatalf("expected ErrInvalidFileType for late invalid UTF-8, got %v", err)
	}
}

func TestFileStoreSizeQuotaIsolationRefsAndCleanup(t *testing.T) {
	ctx := context.Background()
	db, fs, root := openTestFileStore(t, 100, 150, time.Hour)

	if _, err := fs.Save(ctx, "user-1", "too-big.md", "user_data", strings.NewReader(strings.Repeat("a", 101))); !errors.Is(err, storage.ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got %v", err)
	}

	r1, err := fs.Save(ctx, "user-1", "a.md", "user_data", strings.NewReader(strings.Repeat("a", 90)))
	if err != nil {
		t.Fatalf("Save first file: %v", err)
	}
	if _, err := fs.Save(ctx, "user-1", "b.md", "user_data", strings.NewReader(strings.Repeat("b", 70))); !errors.Is(err, storage.ErrStorageQuota) {
		t.Fatalf("expected ErrStorageQuota (90+70>150), got %v", err)
	}

	// Cross-user isolation checks.
	if _, err := fs.Get(ctx, "user-2", r1.ID); !errors.Is(err, storage.ErrFileForbidden) {
		t.Fatalf("expected ErrFileForbidden on Get by wrong owner, got %v", err)
	}
	if _, _, err := fs.OpenContent(ctx, "user-2", r1.ID); !errors.Is(err, storage.ErrFileForbidden) {
		t.Fatalf("expected ErrFileForbidden on OpenContent by wrong owner, got %v", err)
	}
	if err := fs.Delete(ctx, "user-2", r1.ID); !errors.Is(err, storage.ErrFileForbidden) {
		t.Fatalf("expected ErrFileForbidden on Delete by wrong owner, got %v", err)
	}
	if _, err := fs.Get(ctx, "user-1", "file_nonexistent_123"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}

	// Active run ref prevents deletion (409 conflict behavior).
	lockedRecs, err := fs.AcquireForRun(ctx, "user-1", []string{r1.ID})
	if err != nil {
		t.Fatalf("AcquireForRun: %v", err)
	}
	if err := fs.Delete(ctx, "user-1", r1.ID); !errors.Is(err, storage.ErrFileInUse) {
		t.Fatalf("expected ErrFileInUse while file acquired, got %v", err)
	}

	// CopyToWorkspace writes into explicit workspace input path.
	inputDir := filepath.Join(root, "runs", "run_1", "input")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	destFile := filepath.Join(inputDir, lockedRecs[0].Filename)
	if err := fs.CopyToWorkspace(lockedRecs[0], destFile); err != nil {
		t.Fatalf("CopyToWorkspace: %v", err)
	}
	stagedBytes, err := os.ReadFile(destFile)
	if err != nil || string(stagedBytes) != strings.Repeat("a", 90) {
		t.Fatalf("staged content mismatch: err=%v len=%d", err, len(stagedBytes))
	}

	// Release allows deletion.
	fs.ReleaseForRun(ctx, []string{r1.ID})
	if err := fs.Delete(ctx, "user-1", r1.ID); err != nil {
		t.Fatalf("Delete after ReleaseForRun failed: %v", err)
	}

	// Test TTL cleanup skips nothing when expired and active_refs == 0.
	r2, err := fs.Save(ctx, "user-1", "expire.md", "user_data", strings.NewReader("temp data"))
	if err != nil {
		t.Fatalf("Save r2: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE files SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), r2.ID); err != nil {
		t.Fatalf("force expire: %v", err)
	}
	deleted, err := fs.CleanupExpired(ctx)
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 expired file deleted, got %d", deleted)
	}
	if _, err := fs.Get(ctx, "user-1", r2.ID); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("expected expired file to be removed, got %v", err)
	}
}
