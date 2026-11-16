package tests

import (
	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/pools"
	"cli-ai-gateway/internal/session"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReviewLegacyCooldown(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	if err := h.Pools.SetCooldown(context.Background(), "ollama-local", "qwen2.5-coder:7b", "rate_limit", "quota", time.Now().Add(time.Hour), "parsed"); err != nil {
		t.Fatal(err)
	}
	status, _, _ := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{"model": "ollama-local", "input": "test"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected cooldown HTTP 503, got %d", status)
	}
}

func TestReviewDisabledShadow(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	_, err := h.Pools.UpsertPublishedModel(context.Background(), pools.PublishedModel{ID: "ollama-local", Enabled: false, Targets: []pools.PublishedTarget{{ProviderID: "ollama-local", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, body := doJSONReqWithHeaders(t, http.MethodGet, h.URL+"/v1/models", "alice-secret", nil)
	if strings.Contains(string(body), `"id":"ollama-local"`) {
		t.Fatal("disabled published model still listed through legacy registry")
	}
}

type reviewCooldownExecutor struct {
	executor.Executor
	store *pools.Store
}

func (e reviewCooldownExecutor) Execute(ctx context.Context, req executor.ExecuteRequest, emit executor.EventHandler) (executor.ExecuteResult, error) {
	res, err := e.Executor.Execute(ctx, req, emit)
	// Deterministic ordering: a concurrent request records its 429 before this success returns.
	if cdErr := e.store.SetCooldown(ctx, "ollama-local", "qwen2.5-coder:7b", "rate_limit", "concurrent quota", time.Now().Add(time.Hour), "parsed"); cdErr != nil {
		return res, cdErr
	}
	return res, err
}
func TestReviewSuccessErasesNewCooldown(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	info, _ := h.Registry.GetProvider("ollama-local")
	h.Registry.RegisterFactory("ollama", func(config.ModelConfig) (executor.Executor, error) {
		return reviewCooldownExecutor{info.Executor, h.Pools}, nil
	})
	if err := h.Registry.RegisterModels([]config.ModelConfig{info.Config}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Pools.UpsertPublishedModel(context.Background(), pools.PublishedModel{ID: "review-pool", Enabled: true, Targets: []pools.PublishedTarget{{ProviderID: "ollama-local", ProviderModel: "qwen2.5-coder:7b", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	status, _, body := doJSONReqWithHeaders(t, http.MethodPost, h.URL+"/v1/responses", "alice-secret", map[string]any{"model": "review-pool", "input": "test"})
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	cds, err := h.Pools.ListCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cds) == 0 {
		t.Fatal("successful in-flight request erased newer cooldown")
	}
}
func TestReviewLegacyResumeDifferentWorker(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	sess := session.Session{ID: "legacy", Provider: "codex", Model: "codex-1", ProviderSessionID: "session-on-worker-1"}
	run := session.RunRecord{TargetProviderID: "codex-2", TargetModel: "new-model"}
	sc, _, err := h.Sessions.BuildSessionContext(sess, true, "codex", executor.Capabilities{SessionResume: true}, run)
	if err != nil {
		t.Fatal(err)
	}
	if sc.IsResume {
		t.Fatal("legacy session resumed on different worker instead of restoring history")
	}
}
