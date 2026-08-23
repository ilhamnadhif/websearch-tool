package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ilhamnadhif/websearch-tool/fetchpage"
	"github.com/ilhamnadhif/websearch-tool/websearch"
)

const openRouterAPIKey = ""

const model = "deepseek/deepseek-v4-flash-0731"

const provider = "DeepInfra"

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

const maxToolTurns = 10

const globalTimeout = 5 * time.Minute

const hardTimeout = globalTimeout + 90*time.Second

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s \"pertanyaan kamu\"\n", os.Args[0])
		os.Exit(1)
	}

	question := strings.Join(os.Args[1:], " ")

	ctx, cancel := context.WithTimeout(context.Background(), hardTimeout)
	defer cancel()

	messages := []chatMessage{
		{
			Role: "system",
			Content: "Kamu asisten yang bisa cari info terbaru di internet. " +
				"Pakai tool web_search dulu buat lihat kandidat sumber (title+snippet), " +
				"baru pakai fetch_page untuk baca isi lengkap 1-3 URL yang paling relevan " +
				"sebelum menjawab. Jangan fetch_page semua hasil search sekaligus.",
		},
		{Role: "user", Content: question},
	}

	tools := []toolDef{
		{
			Type: "function",
			Function: functionDef{
				Name:        "web_search",
				Description: "Cari query di internet, balikin daftar judul/url/site/snippet (tanpa isi lengkap halaman).",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Kata kunci pencarian"}
					},
					"required": ["query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: functionDef{
				Name:        "fetch_page",
				Description: "Ambil isi lengkap satu atau beberapa URL (hasil dari web_search).",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"urls": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Daftar URL yang mau dibaca isi lengkapnya"
						}
					},
					"required": ["urls"]
				}`),
			},
		},
	}

	deadline := time.Now().Add(globalTimeout)
	modelCalls := 0
	toolCalls := 0

	for turn := 0; turn < maxToolTurns; turn++ {
		turnTools := tools
		if turn == maxToolTurns-1 || time.Now().After(deadline) {
			turnTools = nil
		}

		modelCalls++
		fmt.Fprintf(os.Stderr, "[model] panggilan API ke-%d\n", modelCalls)

		msg, finishReason, err := callModel(ctx, messages, turnTools)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Gagal manggil model:", err)
			os.Exit(1)
		}

		messages = append(messages, msg)

		if finishReason != "tool_calls" || len(msg.ToolCalls) == 0 {
			fmt.Fprintf(os.Stderr, "[ringkasan] %d panggilan model API, %d tool call\n", modelCalls, toolCalls)
			fmt.Println(msg.Content)
			return
		}

		for _, tc := range msg.ToolCalls {
			toolCalls++
			fmt.Fprintf(os.Stderr, "[tool] %s(%s)\n", tc.Function.Name, tc.Function.Arguments)

			result, err := executeTool(ctx, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				result = fmt.Sprintf(`{"error": %q}`, err.Error())
			}

			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	fmt.Fprintln(os.Stderr, "Berhenti: kelewat banyak iterasi tool-calling.")
	os.Exit(1)
}

func executeTool(ctx context.Context, name, argsJSON string) (string, error) {
	switch name {
	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("argumen web_search tidak valid: %w", err)
		}

		resp, err := websearch.Search(ctx, args.Query)
		if err != nil {
			return "", err
		}

		b, err := json.Marshal(resp)
		return string(b), err

	case "fetch_page":
		var args struct {
			URLs []string `json:"urls"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("argumen fetch_page tidak valid: %w", err)
		}

		results := fetchpage.FetchMany(ctx, args.URLs)

		b, err := json.Marshal(results)
		return string(b), err

	default:
		return "", fmt.Errorf("tool tidak dikenal: %s", name)
	}
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model     string               `json:"model"`
	Messages  []chatMessage        `json:"messages"`
	Tools     []toolDef            `json:"tools,omitempty"`
	Provider  *providerPreference  `json:"provider,omitempty"`
	Reasoning *reasoningPreference `json:"reasoning,omitempty"`
}

type providerPreference struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

type reasoningPreference struct {
	Enabled bool `json:"enabled"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func callModel(ctx context.Context, messages []chatMessage, tools []toolDef) (chatMessage, string, error) {
	reqBody := chatRequest{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		Provider:  &providerPreference{Order: []string{provider}, AllowFallbacks: false},
		Reasoning: &reasoningPreference{Enabled: false},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return chatMessage{}, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(b))
	if err != nil {
		return chatMessage{}, "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+openRouterAPIKey)

	client := &http.Client{Timeout: 60 * time.Second}

	resp, err := client.Do(req)
	if err != nil && ctx.Err() == nil {
		time.Sleep(500 * time.Millisecond)
		req.Body = io.NopCloser(bytes.NewReader(b))

		resp, err = client.Do(req)
	}
	if err != nil {
		return chatMessage{}, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatMessage{}, "", err
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return chatMessage{}, "", fmt.Errorf("gagal parse response (%s): %w", string(body), err)
	}

	if cr.Error != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter error: %s", cr.Error.Message)
	}

	if len(cr.Choices) == 0 {
		return chatMessage{}, "", fmt.Errorf("response tanpa choices: %s", string(body))
	}

	choice := cr.Choices[0]

	return choice.Message, choice.FinishReason, nil
}
