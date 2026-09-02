package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metron/internal/ollama"
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("http://example.invalid/v1/chat/completions", "test-model", DefaultOptions())

	if c.endpoint != "http://example.invalid/v1/chat/completions" || c.model != "test-model" {
		t.Fatalf("NewClient() = %+v, want the endpoint and model stored", c)
	}
	if c.http == nil {
		t.Fatal("NewClient() http client = nil")
	}
	if c.http.Timeout != 0 {
		t.Fatalf("NewClient() http client timeout = %v, want none -- the idle watchdog owns it", c.http.Timeout)
	}
	if c.opts.Timeout != 180*time.Second {
		t.Fatalf("NewClient() idle timeout = %v, want 180s", c.opts.Timeout)
	}
}

func TestNewClientFallsBackToTheDefaultTimeout(t *testing.T) {
	c := NewClient("http://example.invalid", "m", Options{Temperature: 0.2})

	if c.opts.Timeout != DefaultOptions().Timeout {
		t.Fatalf("timeout = %v, want the default %v", c.opts.Timeout, DefaultOptions().Timeout)
	}
}

func TestChatSendsWellFormedNonStreamingRequest(t *testing.T) {
	var (
		gotBody   chatRequest
		gotPath   string
		gotHeader string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotHeader = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("server could not decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message messageWire `json:"message"`
			}{{Message: messageWire{Role: "assistant", Content: "pong"}}},
			Usage: usageWire{PromptTokens: 5, CompletionTokens: 2},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v1/chat/completions", "local-model", DefaultOptions())
	msgs := []ollama.Message{{Role: "user", Content: "ping"}}
	tls := []ollama.Tool{{Type: "function", Function: map[string]any{"name": "find_symbol"}}}

	reply, err := c.Chat(context.Background(), msgs, tls)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if reply.Role != "assistant" || reply.Content != "pong" {
		t.Fatalf("Chat() = %+v, want the assistant message", reply)
	}
	if reply.Usage.PromptTokens != 5 || reply.Usage.GenTokens != 2 {
		t.Fatalf("Usage = %+v, want the counts from the response", reply.Usage)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions" {
		t.Errorf("request = %s %s, want POST /v1/chat/completions", gotMethod, gotPath)
	}
	if gotHeader != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotHeader)
	}
	if gotBody.Model != "local-model" {
		t.Errorf("model = %q, want local-model", gotBody.Model)
	}
	if gotBody.Stream {
		t.Error("stream = true, want false for a non-streaming request")
	}
	if gotBody.StreamOptions != nil {
		t.Error("stream_options set on a non-streaming request, want it omitted")
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "ping" {
		t.Errorf("messages = %+v, want the caller's history", gotBody.Messages)
	}
	if len(gotBody.Tools) != 1 {
		t.Errorf("tools = %+v, want the tool definitions forwarded", gotBody.Tools)
	}
	if gotBody.Temperature != 0.1 || gotBody.TopP != 0.95 {
		t.Errorf("sampling = (%v, %v), want (0.1, 0.95)", gotBody.Temperature, gotBody.TopP)
	}
}

func TestChatConvertsToolCallArgumentsToAJSONString(t *testing.T) {
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	tc := ollama.ToolCall{ID: "call_1"}
	tc.Function.Name = "view_slice"
	tc.Function.Arguments = map[string]any{"path": "a.go", "start": float64(1)}
	msgs := []ollama.Message{{Role: "assistant", ToolCalls: []ollama.ToolCall{tc}}}

	if _, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), msgs, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if len(gotBody.Messages) != 1 || len(gotBody.Messages[0].ToolCalls) != 1 {
		t.Fatalf("request tool calls = %+v, want one forwarded", gotBody.Messages)
	}
	wc := gotBody.Messages[0].ToolCalls[0]
	if wc.ID != "call_1" || wc.Type != "function" || wc.Function.Name != "view_slice" {
		t.Fatalf("tool call = %+v, want id/type/name preserved", wc)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(wc.Function.Arguments), &decoded); err != nil {
		t.Fatalf("Function.Arguments = %q, want valid JSON: %v", wc.Function.Arguments, err)
	}
	if decoded["path"] != "a.go" {
		t.Fatalf("decoded arguments = %+v, want path preserved", decoded)
	}
}

func TestChatSendsToolCallIDOnToolMessages(t *testing.T) {
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	msgs := []ollama.Message{{Role: "tool", ToolCallID: "call_1", Content: "result"}}
	if _, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), msgs, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if len(gotBody.Messages) != 1 || gotBody.Messages[0].ToolCallID != "call_1" {
		t.Fatalf("request messages = %+v, want tool_call_id forwarded", gotBody.Messages)
	}
}

func TestChatDecodesToolCallsFromTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"",`+
			`"tool_calls":[{"id":"call_9","type":"function","function":{"name":"list_files","arguments":"{\"pattern\":\"*.go\"}"}}]}}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	reply, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want one decoded", reply.ToolCalls)
	}
	tc := reply.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "list_files" {
		t.Fatalf("tool call = %+v, want id and name decoded", tc)
	}
	if tc.Function.Arguments["pattern"] != "*.go" {
		t.Fatalf("arguments = %+v, want the JSON string decoded into a map", tc.Function.Arguments)
	}
}

func TestChatReportsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("Chat() error = %v, want the status and body reported", err)
	}
}

func TestChatReportsMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("Chat() error = %v, want the decode failure reported", err)
	}
}

func TestChatReportsNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("Chat() error = %v, want the empty-choices failure reported", err)
	}
}

func TestChatReportsTransportFailure(t *testing.T) {
	_, err := NewClient("http://127.0.0.1:1/v1/chat/completions", "m", DefaultOptions()).
		Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "openai http post") {
		t.Fatalf("Chat() error = %v, want the transport failure reported", err)
	}
}

func TestChatHonoursContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(ctx, nil, nil)
	if err == nil {
		t.Fatal("Chat() error = nil, want context cancellation reported")
	}
}

func TestChatIdleTimeoutCancelsAStalledRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	opts := DefaultOptions()
	opts.Timeout = 50 * time.Millisecond
	_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Chat() = nil error, want the idle watchdog to give up")
	}
}

// sseServer writes a scripted sequence of SSE "data:" lines, one per string,
// terminated by "data: [DONE]".
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func streamOpts(sink io.Writer) Options {
	o := DefaultOptions()
	o.Stream = true
	o.Sink = sink
	return o
}

func TestChatAssemblesAStreamedReply(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Greet "}}]}`,
		`{"choices":[{"delta":{"content":"returns "}}]}`,
		`{"choices":[{"delta":{"content":"hola."}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}`,
	})
	defer srv.Close()

	var sink bytes.Buffer
	reply, err := NewClient(srv.URL, "m", streamOpts(&sink)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if reply.Content != "Greet returns hola." {
		t.Fatalf("Content = %q, want the chunks joined", reply.Content)
	}
	if reply.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", reply.Role)
	}
	if sink.String() != "Greet returns hola." {
		t.Fatalf("sink = %q, want the content echoed as it arrived", sink.String())
	}
	if reply.Usage.PromptTokens != 12 || reply.Usage.GenTokens != 5 {
		t.Fatalf("Usage = %+v, want the counts from the usage chunk", reply.Usage)
	}
}

func TestChatSendsStreamOptionsWhenStreaming(t *testing.T) {
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !gotBody.Stream {
		t.Error("stream = false, want true")
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage not set, want it requested so usage is reported")
	}
}

func TestChatAssemblesInterleavedStreamedToolCalls(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[` +
			`{"index":0,"id":"call_a","function":{"name":"find_symbol","arguments":""}},` +
			`{"index":1,"id":"call_b","function":{"name":"list_files","arguments":""}}` +
			`]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"sym"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"pattern\":\"*.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"bol\":\"Greet\"}"}}]}}]}`,
	})
	defer srv.Close()

	reply, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(reply.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want two assembled by index", reply.ToolCalls)
	}
	if reply.ToolCalls[0].ID != "call_a" || reply.ToolCalls[0].Function.Name != "find_symbol" {
		t.Fatalf("ToolCalls[0] = %+v, want call_a/find_symbol", reply.ToolCalls[0])
	}
	if reply.ToolCalls[0].Function.Arguments["symbol"] != "Greet" {
		t.Fatalf("ToolCalls[0].Arguments = %+v, want the fragmented JSON reassembled", reply.ToolCalls[0].Function.Arguments)
	}
	if reply.ToolCalls[1].ID != "call_b" || reply.ToolCalls[1].Function.Arguments["pattern"] != "*.go" {
		t.Fatalf("ToolCalls[1] = %+v, want call_b/list_files with its own arguments", reply.ToolCalls[1])
	}
}

func TestChatStreamsWithoutASink(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":[{"delta":{"role":"assistant","content":"hi"}}]}`})
	defer srv.Close()

	reply, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if reply.Content != "hi" {
		t.Fatalf("Content = %q, want the reply assembled with no sink attached", reply.Content)
	}
}

func TestChatStreamIgnoresBlankAndNonDataLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ": comment\n\n")
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	reply, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if reply.Content != "ok" {
		t.Fatalf("Content = %q, want only the data: line's content", reply.Content)
	}
}

func TestChatEndsAStreamThatStopsWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"partial"}}]}`+"\n\n")
	}))
	defer srv.Close()

	reply, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if reply.Content != "partial" {
		t.Fatalf("Content = %q, want what arrived before EOF", reply.Content)
	}
}

func TestChatReportsAMalformedStreamChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: not json\n\n")
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode stream") {
		t.Fatalf("Chat() error = %v, want the stream decode failure", err)
	}
}

func TestToWireArgumentsFallsBackOnAnUnmarshalableMap(t *testing.T) {
	got := toWireArguments(map[string]any{"bad": make(chan int)})
	if got != "{}" {
		t.Fatalf("toWireArguments() = %q, want the empty-object fallback", got)
	}
}

func TestChatReportsAMarshalFailure(t *testing.T) {
	tools := []ollama.Tool{{Type: "function", Function: make(chan int)}}
	_, err := NewClient("http://example.invalid", "m", DefaultOptions()).Chat(context.Background(), nil, tools)
	if err == nil || !strings.Contains(err.Error(), "marshal request") {
		t.Fatalf("Chat() error = %v, want the marshal failure reported", err)
	}
}

func TestChatReportsInvalidEndpoint(t *testing.T) {
	_, err := NewClient("http://[::1]:namedport/bad", "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create request") {
		t.Fatalf("Chat() error = %v, want the request-construction failure reported", err)
	}
}

// errReader returns some bytes then a non-EOF error, exercising the scanner
// failure path readStream reports distinctly from clean EOF.
type errReader struct {
	data []byte
	sent bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, fmt.Errorf("simulated read failure")
}

func TestReadStreamReportsAScannerFailure(t *testing.T) {
	c := NewClient("http://example.invalid", "m", streamOpts(nil))
	_, err := c.readStream(&errReader{data: []byte(": keep-alive comment, skipped by the scanner\n")}, time.NewTimer(time.Second))
	if err == nil || !strings.Contains(err.Error(), "read stream") {
		t.Fatalf("readStream() error = %v, want the scanner failure reported", err)
	}
}
