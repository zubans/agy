package antigravity

import (
	"slices"
	"testing"

	"cli-ai-gateway/internal/config"
	"cli-ai-gateway/internal/executor"
)

func TestParseAgyModelsOutput_TabSeparated(t *testing.T) {
	raw := `
ID	NAME
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)	default
claude-sonnet-4-6-thinking	Claude Sonnet 4.6 (Thinking)
`
	got := parseAgyModelsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 models, got %d: %+v", len(got), got)
	}
	if got[0].ID != "gemini-3.8-flash-low" || got[0].Name != "Gemini 3.8 Flash (Low)" || got[0].Default {
		t.Errorf("unexpected model[0]: %+v", got[0])
	}
	if got[1].ID != "gemini-3.8-flash-medium" || got[1].Name != "Gemini 3.8 Flash (Medium)" || !got[1].Default {
		t.Errorf("unexpected model[1]: %+v", got[1])
	}
	if got[2].ID != "claude-sonnet-4-6-thinking" || got[2].Name != "Claude Sonnet 4.6 (Thinking)" {
		t.Errorf("unexpected model[2]: %+v", got[2])
	}
}

func TestParseAgyModelsOutput_MultiSpaceAndOtherFormats(t *testing.T) {
	raw := `
Available models:
  gemini-3.8-flash-low    Gemini 3.8 Flash (Low)
  * claude-opus-4-6 (Claude Opus 4.6)
  Gemini 3.1 Pro (gemini-3.1-pro)
  gpt-oss-120b - GPT-OSS 120B (Medium)
`
	got := parseAgyModelsOutput(raw)
	if len(got) != 4 {
		t.Fatalf("expected 4 models, got %d: %+v", len(got), got)
	}
	want := []struct {
		id   string
		name string
	}{
		{"gemini-3.8-flash-low", "Gemini 3.8 Flash (Low)"},
		{"claude-opus-4-6", "Claude Opus 4.6"},
		{"gemini-3.1-pro", "Gemini 3.1 Pro"},
		{"gpt-oss-120b", "GPT-OSS 120B (Medium)"},
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Name != w.name {
			t.Errorf("model[%d] = {%q, %q}, want {%q, %q}", i, got[i].ID, got[i].Name, w.id, w.name)
		}
	}
}

func TestBuildArgs_NormalizesTabbedProviderModel(t *testing.T) {
	exec := New(config.ModelConfig{
		ID:       "agy-1",
		Provider: "antigravity",
		Model:    "gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)",
	}, nil)

	argsFromReq := exec.buildArgs(executor.ExecuteRequest{
		ProviderModel: "gemini-3.8-flash-low\tGemini 3.8 Flash (Low)",
	})
	wantReq := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-3.8-flash-low"}
	if !slices.Equal(argsFromReq, wantReq) {
		t.Fatalf("buildArgs(req) = %#v, want %#v", argsFromReq, wantReq)
	}

	argsFromCfg := exec.buildArgs(executor.ExecuteRequest{})
	wantCfg := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-3.8-flash-medium"}
	if !slices.Equal(argsFromCfg, wantCfg) {
		t.Fatalf("buildArgs(cfg) = %#v, want %#v", argsFromCfg, wantCfg)
	}

	argsPlain := exec.buildArgs(executor.ExecuteRequest{
		ProviderModel: "Gemini 3.8 Flash (Low)",
	})
	wantPlain := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--model", "Gemini 3.8 Flash (Low)"}
	if !slices.Equal(argsPlain, wantPlain) {
		t.Fatalf("buildArgs(plain) = %#v, want %#v", argsPlain, wantPlain)
	}
}
