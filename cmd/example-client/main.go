package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"cli-ai-gateway/pkg/client"
)

func main() {
	baseURL := flag.String("url", "http://127.0.0.1:8080", "Gateway base URL")
	apiKey := flag.String("key", "gw-dev-change-me", "Gateway Bearer API key")
	model := flag.String("model", "codex", "Target model ID (codex, antigravity, gemini, claude, local-qwen)")
	stream := flag.Bool("stream", true, "Use SSE streaming")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	gw := client.New(*baseURL, *apiKey)

	models, err := gw.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ListModels failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Available models (%d):\n", len(models))
	for _, m := range models {
		fmt.Printf("  - %s (owned_by=%s)\n", m.ID, m.OwnedBy)
	}

	doc := client.PromptDocument{
		SystemInstructions: "You are a concise software engineering assistant.",
		Task:               "Summarize the three architectural principles in Additional Context.",
		AdditionalContext:  "1. Single Go binary.\n2. File-based prompt transport via OpenAI Files API.\n3. Isolated per-run workspaces.",
		ExpectedOutput:     "A short bullet list with three items.",
	}

	uploaded, err := gw.UploadPrompt(ctx, "prompt.md", doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "UploadPrompt failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Uploaded prompt.md -> %s (%d bytes)\n", uploaded.ID, uploaded.Bytes)

	params := client.ResponseParams{
		Model:   *model,
		FileIDs: []string{uploaded.ID},
		Text:    "Execute the task described in prompt.md.",
	}

	if *stream {
		fmt.Println("Streaming response:")
		resp, err := gw.StreamResponse(ctx, params, func(delta string) {
			fmt.Print(delta)
		})
		fmt.Println()
		if err != nil {
			fmt.Fprintf(os.Stderr, "StreamResponse failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Completed response id=%s (tokens=%d)\n", resp.ID, resp.Usage.TotalTokens)
		return
	}

	resp, err := gw.CreateResponse(ctx, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CreateResponse failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Response id=%s:\n%s\n", resp.ID, resp.OutputText())
}
