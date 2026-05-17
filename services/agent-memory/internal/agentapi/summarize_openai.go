// Package agentapi: OpenAI-API-compatible HTTPS summariser.
//
// Stage 5.4 brief: "The summariser is reached through a
// pluggable `Summariser` client interface (vendor pin for
// v1: any OpenAI-API-compatible HTTPS endpoint is supported
// via config — self-hosted vLLM or external API — so
// deployments pick the vendor at deploy time without code
// changes)."
//
// This file is the v1 vendor-pin: a minimal client against
// the `/v1/chat/completions` endpoint shape OpenAI defined
// and self-hosted vLLM / Ollama / TGI / Together / Anyscale
// inherited. The client implements `Summariser` so the
// summarize verb can wire it without knowing the wire
// protocol.
//
// Design constraints
// ------------------
//   - NO vendor-specific SDK. The protocol surface is small
//     enough that hand-rolling keeps the dependency tree
//     clean; pulling `github.com/openai/openai-go` would
//     drag in many transitive deps for a single POST.
//   - Bearer-token auth ONLY. mTLS / signed-headers are
//     supported by passing a custom `*http.Client` whose
//     `Transport` already does the work.
//   - Single-request, single-response. Streaming is NOT
//     supported in v1 — the summarize verb's 5 s budget is
//     too short for partial-stream rendering to add value,
//     and the `SummariserOutput` shape is a complete
//     string, not a chunk channel.
//   - The client respects the caller's `ctx`: cancellation
//     aborts the in-flight HTTP request via
//     `http.NewRequestWithContext`. The verb wraps every
//     call in a 5 s `context.WithTimeout` so the client
//     does not need to re-impose one.
//
// Why a separate file (not summarize.go)
// --------------------------------------
// `summarize.go` is the abstract verb logic; this file is
// one concrete `Summariser` implementation. Future vendors
// would land as siblings (`summarize_anthropic.go`,
// `summarize_vllm_grpc.go`, etc.) without touching the
// verb file.
package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatibleSummariser is the v1 vendor-pin
// implementation of `Summariser`. Construct via
// `NewOpenAICompatibleSummariser`.
//
// Concurrency: safe for use by multiple goroutines (the
// underlying `*http.Client` is safe; the struct holds no
// mutable state post-construction).
type OpenAICompatibleSummariser struct {
	endpoint   string
	apiKey     string
	model      string
	httpClient *http.Client
	systemMsg  string
}

// OpenAICompatibleConfig parameterises
// `NewOpenAICompatibleSummariser`. Pulled into a struct so
// callers can populate it from env vars / config files in
// one expression without re-arguing the parameter order
// every time.
type OpenAICompatibleConfig struct {
	// Endpoint is the API base URL, WITHOUT the
	// `/chat/completions` suffix. e.g.
	// `https://api.openai.com/v1`, `http://localhost:8000/v1`
	// for vLLM, `https://api.together.xyz/v1` for Together,
	// etc. The trailing slash is tolerated; the client trims
	// it.
	Endpoint string
	// APIKey is the bearer token sent as
	// `Authorization: Bearer <APIKey>`. May be empty for
	// self-hosted endpoints that do not enforce auth (the
	// client OMITS the header in that case so a self-hosted
	// vLLM does not see a misleading bearer prefix).
	APIKey string
	// Model is the model identifier the API expects (e.g.
	// "gpt-4o-mini", "Qwen2.5-7B-Instruct"). Surfaces
	// verbatim on `ModelVersion()`.
	Model string
	// HTTPClient is the underlying client. Optional; nil
	// selects a sensible default with a 30 s request
	// timeout (the verb's per-call 5 s ctx wrapper
	// supersedes this, but the default protects against a
	// caller that forgets to set a deadline).
	HTTPClient *http.Client
	// SystemMessage is the system-role preamble prepended
	// to every request. Optional; empty means no system
	// message. The summarize verb's prompt is already
	// task-specific so a system message is rarely needed.
	SystemMessage string
}

// NewOpenAICompatibleSummariser constructs the v1 vendor-
// pin client. Validates the minimum-required fields:
// `Endpoint` and `Model` MUST be non-empty.
func NewOpenAICompatibleSummariser(cfg OpenAICompatibleConfig) (*OpenAICompatibleSummariser, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("agentapi: OpenAICompatibleSummariser: empty endpoint")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("agentapi: OpenAICompatibleSummariser: empty model")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OpenAICompatibleSummariser{
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		httpClient: client,
		systemMsg:  cfg.SystemMessage,
	}, nil
}

// ModelVersion implements Summariser.
func (s *OpenAICompatibleSummariser) ModelVersion() string {
	return s.model
}

// Summarize implements Summariser. POSTs a single
// `/chat/completions` request and decodes the first choice's
// `message.content`. Returns:
//
//   - `(out, nil)` on a 2xx response with non-empty content.
//   - The wrapped error on any failure: network, non-2xx
//     status, malformed JSON, empty choices. The Stage 5.4
//     verb degrades to the template on every non-nil error
//     except parent-ctx cancellation (which `hardCancellation`
//     in summarize.go re-routes as a hard error).
func (s *OpenAICompatibleSummariser) Summarize(
	ctx context.Context, in SummariserInput,
) (SummariserOutput, error) {
	body, err := s.buildRequestBody(in)
	if err != nil {
		return SummariserOutput{}, err
	}
	url := s.endpoint + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return SummariserOutput{}, fmt.Errorf("agentapi: OpenAICompatibleSummariser: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return SummariserOutput{}, fmt.Errorf("agentapi: OpenAICompatibleSummariser: do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, openAIMaxResponseBytes))
	if err != nil {
		return SummariserOutput{}, fmt.Errorf("agentapi: OpenAICompatibleSummariser: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Truncate the snippet so a verbose server error
		// does not blow up downstream log lines.
		snippet := string(bodyBytes)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return SummariserOutput{}, fmt.Errorf(
			"agentapi: OpenAICompatibleSummariser: status %d: %s",
			resp.StatusCode, snippet,
		)
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
		return SummariserOutput{}, fmt.Errorf(
			"agentapi: OpenAICompatibleSummariser: decode response: %w", err,
		)
	}
	if len(decoded.Choices) == 0 {
		return SummariserOutput{}, errors.New(
			"agentapi: OpenAICompatibleSummariser: response carried zero choices",
		)
	}
	content := decoded.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return SummariserOutput{}, errors.New(
			"agentapi: OpenAICompatibleSummariser: choice 0 carried empty content",
		)
	}
	return SummariserOutput{SummaryMD: content}, nil
}

// buildRequestBody marshals the chat-completions request
// payload. The system message (when configured) is the
// first message; the verb-rendered prompt is always the
// user message. Temperature defaults to 0.2 — low but
// non-zero so the model can still recover from a slightly
// awkward prompt phrasing while staying mostly
// deterministic across reruns.
func (s *OpenAICompatibleSummariser) buildRequestBody(in SummariserInput) ([]byte, error) {
	msgs := make([]openAIChatMessage, 0, 2)
	if s.systemMsg != "" {
		msgs = append(msgs, openAIChatMessage{Role: "system", Content: s.systemMsg})
	}
	msgs = append(msgs, openAIChatMessage{Role: "user", Content: in.Prompt})
	req := openAIChatRequest{
		Model:       s.model,
		Messages:    msgs,
		MaxTokens:   in.MaxTokens,
		Temperature: 0.2,
	}
	return json.Marshal(req)
}

// openAIChatRequest is the minimal POST body shape the
// `/v1/chat/completions` endpoint requires. Streaming /
// tool-calls / response_format fields are intentionally
// omitted — the summarize verb only needs a single
// completion string.
type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIChatResponse is the minimal decode shape. Vendor
// extras (usage / system_fingerprint / etc.) are ignored
// per JSON-decoder defaults.
type openAIChatResponse struct {
	Choices []openAIChatChoice `json:"choices"`
}

type openAIChatChoice struct {
	Message openAIChatMessage `json:"message"`
}

// openAIMaxResponseBytes caps the per-response body the
// client will buffer. 1 MiB is several orders of magnitude
// larger than any reasonable summary; the cap protects
// against a misbehaving endpoint that streams unbounded
// JSON.
const openAIMaxResponseBytes = 1 << 20
