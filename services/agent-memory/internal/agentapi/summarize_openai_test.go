package agentapi

// Unit tests for the v1 OpenAI-API-compatible summariser
// client (summarize_openai.go). Uses `httptest.Server` so
// no network egress is required — the test exercises:
//
//   * happy path: 200 + parsed content
//   * non-200 status: error wraps the body snippet
//   * malformed JSON: decode error surfaces
//   * empty choices array: explicit error (not a panic)
//   * empty content string: explicit error
//   * Bearer header is set when APIKey != ""
//   * Bearer header is OMITTED when APIKey == ""
//   * ctx cancellation aborts the in-flight request
//   * constructor rejects empty Endpoint / Model
//   * trailing slash on Endpoint is tolerated

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleSummariser_happyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q; want /chat/completions", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q; want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("Authorization = %q; want Bearer secret-key", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req openAIChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q; want test-model", req.Model)
		}
		if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "user" {
			t.Errorf("expected user message as last entry; got %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChatChoice{
				{Message: openAIChatMessage{Role: "assistant", Content: "## Summary\nbody"}},
			},
		})
	}))
	defer srv.Close()

	s, err := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL,
		APIKey:   "secret-key",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleSummariser: %v", err)
	}
	out, err := s.Summarize(context.Background(), SummariserInput{
		Prompt: "render me", MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out.SummaryMD != "## Summary\nbody" {
		t.Fatalf("SummaryMD = %q; want canned response", out.SummaryMD)
	}
	if s.ModelVersion() != "test-model" {
		t.Fatalf("ModelVersion = %q; want test-model", s.ModelVersion())
	}
}

func TestOpenAICompatibleSummariser_systemMessagePrepended(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIChatRequest
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) != 2 {
			t.Errorf("messages len = %d; want 2 (system + user)", len(req.Messages))
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != "you are a helpful assistant" {
			t.Errorf("Messages[0] = %+v; want system preamble", req.Messages[0])
		}
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChatChoice{{Message: openAIChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m", SystemMessage: "you are a helpful assistant",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
}

func TestOpenAICompatibleSummariser_omitsBearerWhenNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q; want empty (no API key configured)", got)
		}
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChatChoice{{Message: openAIChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
}

func TestOpenAICompatibleSummariser_non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"backend hiccup"}}`))
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err == nil {
		t.Fatalf("err = nil; want non-200 error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v; want to contain `500`", err)
	}
	if !strings.Contains(err.Error(), "backend hiccup") {
		t.Fatalf("err = %v; want to contain body snippet", err)
	}
}

func TestOpenAICompatibleSummariser_badJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json at all`))
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err == nil {
		t.Fatalf("err = nil; want decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v; want to mention decode failure", err)
	}
}

func TestOpenAICompatibleSummariser_emptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(openAIChatResponse{Choices: nil})
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err == nil {
		t.Fatalf("err = nil; want empty-choices error")
	}
	if !strings.Contains(err.Error(), "zero choices") {
		t.Fatalf("err = %v; want to mention zero choices", err)
	}
}

func TestOpenAICompatibleSummariser_emptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChatChoice{{Message: openAIChatMessage{Content: "   "}}},
		})
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	_, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"})
	if err == nil {
		t.Fatalf("err = nil; want empty-content error")
	}
	if !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("err = %v; want to mention empty content", err)
	}
}

func TestOpenAICompatibleSummariser_ctxCancellation(t *testing.T) {
	// The handler waits up to 5s but unblocks promptly on
	// ctx cancellation so httptest.Server.Close() does not
	// itself hang (Windows connection lifecycle differs
	// from Linux — a half-open TCPConn here can block
	// Server.Close indefinitely if the handler is parked
	// on a bare channel receive).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	s, _ := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL, Model: "m",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Summarize(ctx, SummariserInput{Prompt: "p"})
	if err == nil {
		t.Fatalf("err = nil; want ctx-derived failure")
	}
	srv.CloseClientConnections()
}

func TestNewOpenAICompatibleSummariser_rejectsEmptyEndpoint(t *testing.T) {
	_, err := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: "", Model: "m",
	})
	if err == nil {
		t.Fatalf("err = nil; want validation rejection")
	}
}

func TestNewOpenAICompatibleSummariser_rejectsEmptyModel(t *testing.T) {
	_, err := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: "http://example.com", Model: "",
	})
	if err == nil {
		t.Fatalf("err = nil; want validation rejection")
	}
}

func TestNewOpenAICompatibleSummariser_trimsTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q; want /chat/completions (trailing slash should NOT double up)", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChatChoice{{Message: openAIChatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()
	s, err := NewOpenAICompatibleSummariser(OpenAICompatibleConfig{
		Endpoint: srv.URL + "/", Model: "m",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleSummariser: %v", err)
	}
	if _, err := s.Summarize(context.Background(), SummariserInput{Prompt: "p"}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
}
