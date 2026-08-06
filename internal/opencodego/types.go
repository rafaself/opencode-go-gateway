// Package opencodego contains the OpenCode Go Chat Completions wire client.
//
// Bridge values are translated into provider-specific messages here. The
// package intentionally does not import internal/codex and does not decode
// SSE; callers own a successful response body and the next milestone performs
// incremental stream translation.
package opencodego

import (
	"encoding/json"
	"io"
	"net/http"
)

const (
	DefaultBaseURL                   = "https://opencode.ai/zen/go/v1"
	DefaultModel                     = "deepseek-v4-flash"
	DeepSeekV4ProModel               = "deepseek-v4-pro"
	DefaultUserAgent                 = "opencode-go-gateway/dev"
	DefaultAccept                    = "text/event-stream"
	DefaultMaxRequestBodyBytes int64 = 16 << 20
	DefaultMaxErrorBodyBytes   int64 = 64 << 10
)

// ThinkingMode controls the provider extension used by DeepSeek-compatible
// Chat Completions. The zero value means the provider-default MVP policy,
// which is explicit thinking enabled because the documented provider default
// is enabled.
type ThinkingMode string

const (
	ThinkingDefault  ThinkingMode = ""
	ThinkingEnabled  ThinkingMode = "enabled"
	ThinkingDisabled ThinkingMode = "disabled"
)

// ChatCompletionRequest is the provider-owned outbound request wire shape.
type ChatCompletionRequest struct {
	Model             string               `json:"model"`
	Messages          []ChatMessage        `json:"messages"`
	Tools             []ChatCompletionTool `json:"tools,omitempty"`
	ToolChoice        *ToolChoice          `json:"tool_choice,omitempty"`
	ParallelToolCalls bool                 `json:"parallel_tool_calls"`
	Stream            bool                 `json:"stream"`
	StreamOptions     *StreamOptions       `json:"stream_options,omitempty"`
	Thinking          *ThinkingOptions     `json:"thinking,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	ResponseFormat    *ResponseFormat      `json:"response_format,omitempty"`
}

// ChatMessage is shared by non-streaming responses and streaming deltas.
// ReasoningContent remains provider metadata and is never converted to bridge
// text by this package.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the explicit assistant-side function-call representation.
type ToolCall struct {
	// Index is populated by streamed provider deltas. It is a pointer because
	// the provider omits the field from non-streaming tool calls, while index 0
	// is a meaningful position for reconstructing interleaved parallel calls.
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletionTool wraps a function declaration in the provider's tool
// union. Parameters is retained as raw JSON so schema bytes are not
// reinterpreted or lost before a later request/continuation milestone.
type ChatCompletionTool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// Tool is a concise alias useful to callers constructing provider fixtures.
type Tool = ChatCompletionTool

type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolChoice supports the provider's string and named-function forms.
// Exactly one of String or Type should be set by request mapping.
type ToolChoice struct {
	String   string
	Type     string
	Function ToolChoiceFunction
}

type ToolChoiceFunction struct {
	Name string `json:"name"`
}

func (choice ToolChoice) MarshalJSON() ([]byte, error) {
	if choice.String != "" {
		return json.Marshal(choice.String)
	}
	return json.Marshal(struct {
		Type     string             `json:"type"`
		Function ToolChoiceFunction `json:"function"`
	}{Type: choice.Type, Function: choice.Function})
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ThinkingOptions struct {
	Type string `json:"type"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

// ChatCompletionResponse is the decoded non-streaming wire shape retained for
// future callers. The current client always requests a stream, but keeping
// this type here lets #6/#10 decode provider chunks without inventing a second
// set of assistant metadata types.
type ChatCompletionResponse struct {
	ID                string                 `json:"id"`
	Choices           []ChatCompletionChoice `json:"choices"`
	Created           int64                  `json:"created"`
	Model             string                 `json:"model"`
	SystemFingerprint string                 `json:"system_fingerprint"`
	Object            string                 `json:"object"`
	Usage             *Usage                 `json:"usage"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	FinishReason string      `json:"finish_reason"`
	Message      ChatMessage `json:"message"`
}

type ChatCompletionChunk struct {
	ID                string                      `json:"id"`
	Choices           []ChatCompletionChunkChoice `json:"choices"`
	Created           int64                       `json:"created"`
	Model             string                      `json:"model"`
	SystemFingerprint string                      `json:"system_fingerprint"`
	Object            string                      `json:"object"`
	Usage             *Usage                      `json:"usage"`
}

type ChatCompletionChunkChoice struct {
	Index        int         `json:"index"`
	FinishReason *string     `json:"finish_reason"`
	Delta        ChatMessage `json:"delta"`
}

type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptCacheHitTokens    int                      `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens   int                      `json:"prompt_cache_miss_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Response is returned only after the upstream response has passed status and
// content-type validation. The Body remains open and belongs to the caller;
// Close is an idempotent convenience for that ownership boundary.
type Response struct {
	StatusCode  int
	Header      http.Header
	ContentType string
	Body        io.ReadCloser
}

func (response *Response) Close() error {
	if response == nil || response.Body == nil {
		return nil
	}
	err := response.Body.Close()
	response.Body = nil
	return err
}
