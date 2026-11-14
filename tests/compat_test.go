package tests

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

func TestCompat_OfficialOpenAIGoSDK(t *testing.T) {
	h := newTestHarness(t, harnessOptions{})
	ctx := context.Background()

	sdk := openai.NewClient(
		option.WithBaseURL(h.URL+"/v1"),
		option.WithAPIKey("alice-secret"),
	)

	// 1. Models.List
	modelsPage, err := sdk.Models.List(ctx)
	if err != nil {
		t.Fatalf("sdk.Models.List failed: %v", err)
	}
	if len(modelsPage.Data) < 5 {
		t.Fatalf("expected >=5 models via SDK, got %d", len(modelsPage.Data))
	}

	// 2. Files.New + Get + Content
	promptMD := "# Task\nSTART_MARKER_Alpha\nCheck SDK file compatibility.\nEND_MARKER_Gamma\n"
	fileObj, err := sdk.Files.New(ctx, openai.FileNewParams{
		File:    openai.File(strings.NewReader(promptMD), "prompt.md", "text/markdown"),
		Purpose: openai.FilePurposeUserData,
	})
	if err != nil {
		t.Fatalf("sdk.Files.New failed: %v", err)
	}
	if fileObj.ID == "" || fileObj.Filename != "prompt.md" {
		t.Fatalf("unexpected uploaded FileObject: %+v", fileObj)
	}

	gotFile, err := sdk.Files.Get(ctx, fileObj.ID)
	if err != nil || gotFile.ID != fileObj.ID {
		t.Fatalf("sdk.Files.Get failed: %v (%+v)", err, gotFile)
	}

	contentResp, err := sdk.Files.Content(ctx, fileObj.ID)
	if err != nil {
		t.Fatalf("sdk.Files.Content failed: %v", err)
	}
	contentBytes, _ := io.ReadAll(contentResp.Body)
	_ = contentResp.Body.Close()
	if string(contentBytes) != promptMD {
		t.Fatalf("sdk.Files.Content mismatch: %q", string(contentBytes))
	}

	// 3. Responses.New with input_file + input_text + Instructions
	resp1, err := sdk.Responses.New(ctx, responses.ResponseNewParams{
		Model:        "codex-cli",
		Instructions: openai.String("Respond deterministically."),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				responses.ResponseInputItemParamOfInputMessage(
					responses.ResponseInputMessageContentListParam{
						{
							OfInputFile: &responses.ResponseInputFileParam{
								FileID: openai.String(fileObj.ID),
							},
						},
						responses.ResponseInputContentParamOfInputText("Analyze prompt.md via SDK"),
					},
					"user",
				),
			},
		},
	})
	if err != nil {
		t.Fatalf("sdk.Responses.New failed: %v", err)
	}
	if !strings.Contains(resp1.OutputText(), "start=true mid=false end=true") {
		t.Fatalf("expected SDK response text confirming prompt.md markers, got: %s", resp1.OutputText())
	}

	// 4. Responses.NewStreaming with PreviousResponseID
	stream := sdk.Responses.NewStreaming(ctx, responses.ResponseNewParams{
		Model:              "codex-cli",
		PreviousResponseID: openai.String(resp1.ID),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String("Follow-up via streaming SDK"),
		},
	})
	var streamed strings.Builder
	var completedID string
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.output_text.delta":
			streamed.WriteString(ev.AsResponseOutputTextDelta().Delta)
		case "response.completed":
			completedID = ev.AsResponseCompleted().Response.ID
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("sdk.Responses.NewStreaming error: %v", err)
	}
	if completedID == "" || !strings.Contains(streamed.String(), "resume=true") {
		t.Fatalf("unexpected SDK streaming result: completedID=%q text=%q", completedID, streamed.String())
	}

	// 5. Chat.Completions.New (non-streaming) and NewStreaming
	chatResp, err := sdk.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: "codex-cli",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("Short answer."),
			openai.UserMessage("Hello from Chat SDK"),
		},
	})
	if err != nil {
		t.Fatalf("sdk.Chat.Completions.New failed: %v", err)
	}
	if len(chatResp.Choices) == 0 || !strings.Contains(chatResp.Choices[0].Message.Content, "provider=codex") {
		t.Fatalf("unexpected ChatCompletion response: %+v", chatResp)
	}

	chatStream := sdk.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model: "ollama-local",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("Stream from Ollama via Chat SDK"),
		},
	})
	var chatStreamed strings.Builder
	for chatStream.Next() {
		chunk := chatStream.Current()
		if len(chunk.Choices) > 0 {
			chatStreamed.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := chatStream.Err(); err != nil {
		t.Fatalf("sdk.Chat.Completions.NewStreaming error: %v", err)
	}
	if !strings.Contains(chatStreamed.String(), "provider=ollama") {
		t.Fatalf("unexpected streamed ChatCompletion text: %q", chatStreamed.String())
	}

	// 6. Files.Delete
	delRes, err := sdk.Files.Delete(ctx, fileObj.ID)
	if err != nil || !delRes.Deleted {
		t.Fatalf("sdk.Files.Delete failed: %v (%+v)", err, delRes)
	}
}
