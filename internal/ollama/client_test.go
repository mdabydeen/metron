package ollama

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

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("http://example.invalid/api/chat", "test-model", DefaultOptions())

	if c.endpoint != "http://example.invalid/api/chat" || c.model != "test-model" {
		t.Fatalf("NewClient() = %+v, want the endpoint and model stored", c)
	}
	if c.http == nil || c.http.Timeout != 180*time.Second {
		t.Fatalf("NewClient() http client = %+v, want a 180s timeout", c.http)
	}
}

func TestChatSendsWellFormedRequest(t *testing.T) {
	var (
		gotBody   ChatRequest
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
		json.NewEncoder(w).Encode(ChatResponse{
			Message: Message{Role: "assistant", Content: "pong"},
			Done:    true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/api/chat", "qwen-test", DefaultOptions())
	msgs := []Message{{Role: "user", Content: "ping"}}
	tls := []Tool{{Type: "function", Function: map[string]any{"name": "find_symbol"}}}

	msg, err := c.Chat(context.Background(), msgs, tls)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if msg.Role != "assistant" || msg.Content != "pong" {
		t.Fatalf("Chat() = %+v, want the assistant message", msg)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/chat" {
		t.Errorf("request = %s %s, want POST /api/chat", gotMethod, gotPath)
	}
	if gotHeader != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotHeader)
	}
	if gotBody.Model != "qwen-test" {
		t.Errorf("model = %q, want qwen-test", gotBody.Model)
	}
	if gotBody.Stream {
		t.Error("stream = true, want false (the agent reads whole replies)")
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "ping" {
		t.Errorf("messages = %+v, want the caller's history", gotBody.Messages)
	}
	if len(gotBody.Tools) != 1 {
		t.Errorf("tools = %+v, want the tool definitions forwarded", gotBody.Tools)
	}
	for key, want := range map[string]float64{"temperature": 0.1, "top_p": 0.95, "num_ctx": 16384} {
		if got, ok := gotBody.Options[key].(float64); !ok || got != want {
			t.Errorf("options[%q] = %v, want %v", key, gotBody.Options[key], want)
		}
	}
}

func TestChatOmitsToolsWhenEmpty(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&raw)
		json.NewEncoder(w).Encode(ChatResponse{Message: Message{Role: "assistant"}})
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if _, present := raw["tools"]; present {
		t.Errorf("request body = %v, want the tools field omitted", raw)
	}
}

func TestChatDecodesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"role":"assistant","content":"",
			"tool_calls":[{"function":{"name":"view_slice",
			"arguments":{"path":"main.go","start":1,"end":20}}}]},"done":true}`)
	}))
	defer srv.Close()

	msg, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly one", msg.ToolCalls)
	}
	call := msg.ToolCalls[0]
	if call.Function.Name != "view_slice" {
		t.Errorf("name = %q, want view_slice", call.Function.Name)
	}
	if call.Function.Arguments["path"] != "main.go" {
		t.Errorf("arguments = %v, want the decoded path", call.Function.Arguments)
	}
}

func TestChatReportsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "missing", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Chat() = nil error, want a status error")
	}
	if !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("Chat() error = %v, want status and body reported", err)
	}
}

func TestChatReportsMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json")
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("Chat() error = %v, want a decode error", err)
	}
}

func TestChatReportsTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := NewClient(url, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ollama http post") {
		t.Fatalf("Chat() error = %v, want a transport error", err)
	}
}

func TestChatHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(ctx, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ollama http post") {
		t.Fatalf("Chat() error = %v, want the cancelled request reported", err)
	}
}

func TestChatReportsUnmarshalableTools(t *testing.T) {
	c := NewClient("http://example.invalid", "m", DefaultOptions())
	// Functions cannot be JSON-encoded, so marshalling the request must fail.
	bad := []Tool{{Type: "function", Function: func() {}}}

	_, err := c.Chat(context.Background(), nil, bad)
	if err == nil || !strings.Contains(err.Error(), "marshal request") {
		t.Fatalf("Chat() error = %v, want a marshal error", err)
	}
}

func TestChatReportsInvalidEndpoint(t *testing.T) {
	_, err := NewClient(":://not a url", "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create request") {
		t.Fatalf("Chat() error = %v, want a request-construction error", err)
	}
}

func TestNewClientFallsBackToTheDefaultTimeout(t *testing.T) {
	c := NewClient("http://example.invalid", "m", Options{Temperature: 0.2})

	if c.http.Timeout != DefaultOptions().Timeout {
		t.Fatalf("timeout = %v, want the default %v", c.http.Timeout, DefaultOptions().Timeout)
	}
}

func TestChatSendsConfiguredSamplingOptions(t *testing.T) {
	var got ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(ChatResponse{Message: Message{Role: "assistant"}})
	}))
	defer srv.Close()

	opts := Options{Temperature: 0.7, TopP: 0.5, NumCtx: 4096, Timeout: 5 * time.Second}
	if _, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	for key, want := range map[string]float64{"temperature": 0.7, "top_p": 0.5, "num_ctx": 4096} {
		if v, _ := got.Options[key].(float64); v != want {
			t.Errorf("options[%q] = %v, want %v", key, got.Options[key], want)
		}
	}
}
