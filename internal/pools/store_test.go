package pools_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
	"cli-ai-gateway/internal/executor/defaults"
	"cli-ai-gateway/internal/pools"
	"cli-ai-gateway/internal/process"
	"cli-ai-gateway/internal/storage"
)

func setupTestPools(t *testing.T) (*pools.Store, *executor.Registry) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	procMgr := process.NewManager(process.Limits{
		MaxGlobal: 8, MaxPerExecutor: 4, MaxPerUser: 4, MaxQueueSize: 16,
		QueueWaitTimeout: 5 * time.Second, DefaultMaxOutput: 1024 * 1024,
	})
	reg := executor.NewRegistry()
	defaults.RegisterAll(reg, procMgr)
	f := false
	models := []config.ModelConfig{
		{ID: "agy-1", Provider: "antigravity", Executable: "/bin/echo", TimeoutSeconds: 30, MaxConcurrent: 2, FSAccess: "workspace-read", Public: &f},
		{ID: "agy-2", Provider: "antigravity", Executable: "/bin/echo", TimeoutSeconds: 30, MaxConcurrent: 2, FSAccess: "workspace-read", Public: &f},
		{ID: "agy-3", Provider: "antigravity", Executable: "/bin/echo", TimeoutSeconds: 30, MaxConcurrent: 2, FSAccess: "workspace-read", Public: &f},
		{ID: "codex-1", Provider: "codex", Executable: "/bin/echo", TimeoutSeconds: 30, MaxConcurrent: 2, FSAccess: "workspace-read", Public: &f},
		{ID: "claude-1", Provider: "claude", Executable: "/bin/echo", TimeoutSeconds: 30, MaxConcurrent: 2, FSAccess: "workspace-read", Public: &f},
	}
	if err := reg.RegisterModels(models); err != nil {
		t.Fatalf("RegisterModels: %v", err)
	}
	return pools.NewStore(db.SQL()), reg
}

func TestPoolsPriorityRoundRobinAndPerModelCooldown(t *testing.T) {
	ctx := context.Background()
	store, reg := setupTestPools(t)

	// 1. Round-robin pool over 3x agy at Priority 1 + Claude fallback at Priority 2.
	pm, err := store.UpsertPublishedModel(ctx, pools.PublishedModel{
		ID:                     "smart-pro",
		DisplayName:            "Smart Pro Pool",
		Strategy:               pools.StrategyRoundRobin,
		Enabled:                true,
		DefaultCooldownSeconds: 300,
		Targets: []pools.PublishedTarget{
			{ProviderID: "agy-1", ProviderModel: "gemini-2.5-pro", Priority: 1, Enabled: true},
			{ProviderID: "agy-2", ProviderModel: "gemini-2.5-pro", Priority: 1, Enabled: true},
			{ProviderID: "agy-3", ProviderModel: "gemini-2.5-pro", Priority: 1, Enabled: true},
			{ProviderID: "claude-1", ProviderModel: "sonnet", Priority: 2, Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("UpsertPublishedModel: %v", err)
	}
	if pm.Kind != "composite" || len(pm.Targets) != 4 {
		t.Fatalf("unexpected saved model: %+v", pm)
	}

	// Verify round-robin rotation across priority-1 targets while priority-2 always stays at the end.
	var firstPicks []string
	for range 3 {
		sel, err := store.ResolveCandidates(ctx, reg, "smart-pro")
		if err != nil {
			t.Fatal(err)
		}
		if len(sel.Candidates) != 4 || sel.Candidates[3].ProviderID != "claude-1" {
			t.Fatalf("expected 4 candidates ending with claude-1, got %+v", sel.Candidates)
		}
		firstPicks = append(firstPicks, sel.Candidates[0].ProviderID)
	}
	if firstPicks[0] != "agy-1" || firstPicks[1] != "agy-2" || firstPicks[2] != "agy-3" {
		t.Fatalf("expected round-robin agy-1->agy-2->agy-3, got %v", firstPicks)
	}

	// Cool down ONLY (agy-1, gemini-2.5-pro): (agy-1, claude-sonnet-4-5) must remain ready!
	if err := store.SetCooldown(ctx, "agy-1", "gemini-2.5-pro", "rate_limit", "quota", time.Now().Add(time.Hour), "parsed"); err != nil {
		t.Fatal(err)
	}

	_, err = store.UpsertPublishedModel(ctx, pools.PublishedModel{
		ID:       "agy1-claude",
		Enabled:  true,
		Strategy: pools.StrategyPriorityOrder,
		Targets: []pools.PublishedTarget{
			{ProviderID: "agy-1", ProviderModel: "claude-sonnet-4-5", Priority: 1, Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeSel, err := store.ResolveCandidates(ctx, reg, "agy1-claude")
	if err != nil || len(claudeSel.Candidates) != 1 || claudeSel.Candidates[0].ProviderID != "agy-1" {
		t.Fatalf("cooldown on gemini-2.5-pro must not affect claude-sonnet-4-5 on agy-1: %+v err=%v", claudeSel, err)
	}

	// Cool down agy-2 and agy-3 on gemini-2.5-pro -> priority 2 claude-1 must become the first candidate!
	_ = store.SetCooldown(ctx, "agy-2", "gemini-2.5-pro", "rate_limit", "quota", time.Now().Add(time.Hour), "parsed")
	_ = store.SetCooldown(ctx, "agy-3", "gemini-2.5-pro", "rate_limit", "quota", time.Now().Add(time.Hour), "parsed")

	fallbackSel, err := store.ResolveCandidates(ctx, reg, "smart-pro")
	if err != nil {
		t.Fatal(err)
	}
	if len(fallbackSel.Candidates) != 1 || fallbackSel.Candidates[0].ProviderID != "claude-1" {
		t.Fatalf("expected fallback to claude-1 when all P1 targets cool, got %+v", fallbackSel.Candidates)
	}

	// Release agy-1 cooldown -> strictly returns to Priority 1!
	if err := store.ReleaseCooldown(ctx, "agy-1", "gemini-2.5-pro"); err != nil {
		t.Fatal(err)
	}
	recoveredSel, err := store.ResolveCandidates(ctx, reg, "smart-pro")
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredSel.Candidates) != 2 || recoveredSel.Candidates[0].ProviderID != "agy-1" {
		t.Fatalf("expected immediate return to priority 1 agy-1 after cooldown ends, got %+v", recoveredSel.Candidates)
	}
}
