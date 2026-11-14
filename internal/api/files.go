package api

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"cli-ai-gateway/internal/storage"
)

// FilesHandler implements OpenAI-compatible Files endpoints (Section 6.2-6.5).
type FilesHandler struct {
	files        *storage.FileStore
	maxFileBytes int64
}

func NewFilesHandler(files *storage.FileStore, maxFileBytes int64) *FilesHandler {
	return &FilesHandler{
		files:        files,
		maxFileBytes: maxFileBytes,
	}
}

func (h *FilesHandler) Upload(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		writeInvalidRequest(w, "invalid_content_type", "Content-Type must be multipart/form-data", "")
		return
	}

	// Cap request body to maxFileBytes + 2 MiB for multipart headers/boundaries.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxFileBytes+2*1024*1024)
	mr, err := r.MultipartReader()
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", "Request exceeds maximum upload size", nil)
			return
		}
		writeInvalidRequest(w, "invalid_multipart", "Failed to parse multipart form data", "")
		return
	}

	var (
		purpose  string
		hasFile  bool
		uploaded storage.FileRecord
	)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if hasFile {
				_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
			}
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", "Uploaded file exceeds maximum allowed size", nil)
				return
			}
			writeInvalidRequest(w, "invalid_multipart", "Malformed multipart body", "")
			return
		}

		switch part.FormName() {
		case "purpose":
			b, readErr := io.ReadAll(io.LimitReader(part, 256))
			_ = part.Close()
			if readErr != nil {
				if hasFile {
					_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
				}
				writeInvalidRequest(w, "invalid_multipart", "Failed to read purpose field", "purpose")
				return
			}
			purpose = strings.TrimSpace(string(b))
		case "file":
			if hasFile {
				_ = part.Close()
				_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
				writeInvalidRequest(w, "multiple_files", "Only one file part may be uploaded per request", "file")
				return
			}
			// Stream directly to disk with purpose="user_data", then verify actual purpose field below.
			rawFilename := rawMultipartFilename(part)
			rec, saveErr := h.files.Save(r.Context(), principal.OwnerID, rawFilename, "user_data", part)
			_ = part.Close()
			if saveErr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(saveErr, &maxErr) {
					writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", "Uploaded file exceeds maximum allowed size", nil)
					return
				}
				mapStorageError(w, saveErr)
				return
			}
			hasFile = true
			uploaded = rec
		default:
			_ = part.Close()
		}
	}

	if !hasFile {
		writeInvalidRequest(w, "missing_file", "Missing required multipart field 'file'", "file")
		return
	}
	if purpose != "user_data" {
		_ = h.files.Delete(r.Context(), principal.OwnerID, uploaded.ID)
		writeInvalidRequest(w, "invalid_purpose", "Field 'purpose' is required and must be 'user_data'", "purpose")
		return
	}

	if audit := AuditFromContext(r.Context()); audit != nil {
		audit.SetInputBytes(uploaded.Bytes)
	}
	writeJSON(w, http.StatusCreated, uploaded)
}

func (h *FilesHandler) Get(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	fileID := r.PathValue("file_id")

	rec, err := h.files.Get(r.Context(), principal.OwnerID, fileID)
	if err != nil {
		mapStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *FilesHandler) Content(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	fileID := r.PathValue("file_id")

	rec, f, err := h.files.OpenContent(r.Context(), principal.OwnerID, fileID)
	if err != nil {
		mapStorageError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", rec.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (h *FilesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	fileID := r.PathValue("file_id")

	if err := h.files.Delete(r.Context(), principal.OwnerID, fileID); err != nil {
		mapStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fileID,
		"object":  "file",
		"deleted": true,
	})
}

func rawMultipartFilename(part *multipart.Part) string {
	cd := part.Header.Get("Content-Disposition")
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		if raw := params["filename"]; raw != "" {
			return raw
		}
	}
	return part.FileName()
}
