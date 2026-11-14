package api

import (
	"net/http"

	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/storage"
)

// HealthHandler serves GET /health and GET /ready (Section 6.8 - 6.9).
type HealthHandler struct {
	db       *storage.DB
	files    *storage.FileStore
	registry *executor.Registry
}

func NewHealthHandler(db *storage.DB, files *storage.FileStore, registry *executor.Registry) *HealthHandler {
	return &HealthHandler{
		db:       db,
		files:    files,
		registry: registry,
	}
}

func (h *HealthHandler) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready checks SQLite, storage writability, and each configured executor independently.
// Unavailability of one CLI does NOT mark other available executors unhealthy (Section 6.9).
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	dbStatus := "ok"
	coreOK := true
	if err := h.db.Ping(r.Context()); err != nil {
		dbStatus = err.Error()
		coreOK = false
	}

	storageStatus := "ok"
	if err := h.files.CheckWritable(); err != nil {
		storageStatus = err.Error()
		coreOK = false
	}

	execReport := h.registry.CheckAll(r.Context())

	ready := coreOK && (execReport.TotalCount == 0 || execReport.ReadyCount > 0)
	statusText := "ready"
	httpCode := http.StatusOK
	if !ready {
		statusText = "not_ready"
		httpCode = http.StatusServiceUnavailable
	} else if execReport.ReadyCount < execReport.TotalCount {
		statusText = "degraded"
	}

	writeJSON(w, httpCode, map[string]any{
		"status":    statusText,
		"database":  dbStatus,
		"storage":   storageStatus,
		"executors": execReport.Executors,
	})
}
