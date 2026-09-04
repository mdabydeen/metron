package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdabydeen/metron/llm"
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("http://example.invalid/api/chat", "test-model", llm.DefaultOptions())

	if c.endpoint != "http://example.invalid/api/chat" || c.model != "test-model" {
		t.Fatalf("NewClient() = %+v, want the endpoint and model stored", c)
	}
	if c.http == nil {
		t.Fatal("NewClient() http client = nil")
	}
	// The timeout lives on the idle watchdog, not the client: a client-level
	// deadline would cap the whole generation rather than the silence.
	if c.http.Timeout != 0 {
		t.Fatalf("NewClient() http client timeout = %v, want none", c.http.Timeout)
	}
	if c.opts.Timeout != 180*time.Second {
		t.Fatalf("NewClient() idle timeout = %v, want 180s", c.opts.Timeout)
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
			Message: llm.Message{Role: "assistant", Content: "pong"},
			Done:    true,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/api/chat", "qwen-test", llm.DefaultOptions())
	msgs := []llm.Message{{Role: "user", Content: "ping"}}
	tls := []llm.Tool{{Type: "function", Function: map[string]any{"name": "find_symbol"}}}

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
		json.NewEncoder(w).Encode(ChatResponse{Message: llm.Message{Role: "assistant"}})
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil); err != nil {
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

	msg, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
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

	_, err := NewClient(srv.URL, "missing", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
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

	_, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("Chat() error = %v, want a decode error", err)
	}
}

func TestChatReportsTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := NewClient(url, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
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

	_, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(ctx, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ollama http post") {
		t.Fatalf("Chat() error = %v, want the cancelled request reported", err)
	}
}

func TestChatReportsUnmarshalableTools(t *testing.T) {
	c := NewClient("http://example.invalid", "m", llm.DefaultOptions())
	// Functions cannot be JSON-encoded, so marshalling the request must fail.
	bad := []llm.Tool{{Type: "function", Function: func() {}}}

	_, err := c.Chat(context.Background(), nil, bad)
	if err == nil || !strings.Contains(err.Error(), "marshal request") {
		t.Fatalf("Chat() error = %v, want a marshal error", err)
	}
}

func TestChatReportsInvalidEndpoint(t *testing.T) {
	_, err := NewClient(":://not a url", "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create request") {
		t.Fatalf("Chat() error = %v, want a request-construction error", err)
	}
}

func TestNewClientFallsBackToTheDefaultTimeout(t *testing.T) {
	c := NewClient("http://example.invalid", "m", llm.Options{Temperature: 0.2})

	if c.opts.Timeout != llm.DefaultOptions().Timeout {
		t.Fatalf("timeout = %v, want the default %v", c.opts.Timeout, llm.DefaultOptions().Timeout)
	}
}

func TestChatSendsConfiguredSamplingOptions(t *testing.T) {
	var got ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(ChatResponse{Message: llm.Message{Role: "assistant"}})
	}))
	defer srv.Close()

	opts := llm.Options{Temperature: 0.7, TopP: 0.5, NumCtx: 4096, Timeout: 5 * time.Second}
	if _, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	for key, want := range map[string]float64{"temperature": 0.7, "top_p": 0.5, "num_ctx": 4096} {
		if v, _ := got.Options[key].(float64); v != want {
			t.Errorf("options[%q] = %v, want %v", key, got.Options[key], want)
		}
	}
}

func TestChatReportsTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},` +
			`"prompt_eval_count":1240,"eval_count":89,"done":true}`))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Usage.PromptTokens != 1240 || got.Usage.GenTokens != 89 {
		t.Fatalf("Usage = %+v, want the counts Ollama reported", got.Usage)
	}
	if got.Content != "hi" {
		t.Fatalf("Content = %q, want the message still carried", got.Content)
	}
}

func TestChatReportsZeroUsageWhenTheServerOmitsCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},"done":true}`))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if (got.Usage != llm.Usage{}) {
		t.Fatalf("Usage = %+v, want zero when the server reports nothing", got.Usage)
	}
}

func TestUsageAddAccumulates(t *testing.T) {
	u := llm.Usage{PromptTokens: 10, GenTokens: 2}
	u.Add(llm.Usage{PromptTokens: 5, GenTokens: 3})

	if (u != llm.Usage{PromptTokens: 15, GenTokens: 5}) {
		t.Fatalf("Usage = %+v, want the counts summed", u)
	}
}

// streamOpts requests streaming with content echoed to sink.
func streamOpts(sink io.Writer) llm.Options {
	o := llm.DefaultOptions()
	o.Stream = true
	o.Sink = sink
	return o
}

func TestChatAssemblesAStreamedReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !req.Stream {
			t.Error("request Stream = false, want streaming requested")
		}
		for _, chunk := range []string{
			`{"message":{"role":"assistant","content":"Greet "},"done":false}`,
			`{"message":{"role":"assistant","content":"returns "},"done":false}`,
			`{"message":{"role":"assistant","content":"hola."},"done":true,` +
				`"prompt_eval_count":12,"eval_count":5}`,
		} {
			_, _ = w.Write([]byte(chunk + "\n"))
		}
	}))
	defer srv.Close()

	var sink bytes.Buffer
	got, err := NewClient(srv.URL, "m", streamOpts(&sink)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Content != "Greet returns hola." {
		t.Fatalf("Content = %q, want the chunks joined", got.Content)
	}
	if got.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", got.Role)
	}
	if sink.String() != "Greet returns hola." {
		t.Fatalf("sink = %q, want the content echoed as it arrived", sink.String())
	}
	if got.Usage.PromptTokens != 12 || got.Usage.GenTokens != 5 {
		t.Fatalf("Usage = %+v, want the counts from the final chunk", got.Usage)
	}
}

func TestChatCollectsToolCallsFromAStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(
			`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"view_slice",` +
				`"arguments":{"path":"a.go"}}}]},"done":true}` + "\n"))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "view_slice" {
		t.Fatalf("ToolCalls = %+v, want the streamed call assembled", got.ToolCalls)
	}
}

func TestChatStreamsWithoutASink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},"done":true}` + "\n"))
	}))
	defer srv.Close()

	opts := llm.DefaultOptions()
	opts.Stream = true
	got, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Content != "hi" {
		t.Fatalf("Content = %q, want the reply assembled with no sink attached", got.Content)
	}
}

func TestChatEndsAStreamThatStopsWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A server that closes the body mid-stream: EOF ends the reply rather
		// than hanging or erroring.
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"partial"},"done":false}` + "\n"))
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Content != "partial" {
		t.Fatalf("Content = %q, want what arrived before EOF", got.Content)
	}
}

func TestChatReportsAMalformedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json\n"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "m", streamOpts(nil)).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "decode stream") {
		t.Fatalf("Chat() error = %v, want the stream decode failure", err)
	}
}

func TestChatIdleTimeoutCancelsAStalledRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never answers within the timeout
	}))
	defer srv.Close()
	defer close(release)

	opts := llm.DefaultOptions()
	opts.Timeout = 50 * time.Millisecond
	_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)

	if err == nil {
		t.Fatal("Chat() = nil error, want the idle watchdog to give up")
	}
}

func TestStreamIsBounded(t *testing.T) {
	// The idle watchdog resets on every chunk, so a server that streams steadily
	// is never idle and never cancelled -- an out-of-memory condition without a
	// size limit.
	t.Run("content", func(t *testing.T) {
		chunk := `{"message":{"role":"assistant","content":"` + strings.Repeat("x", 60000) + `"}}` + "\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < 200; i++ {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
		}))
		defer srv.Close()
		opts := llm.DefaultOptions()
		opts.Stream = true
		_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("Chat() error = %v, want the reply bounded", err)
		}
	})

	t.Run("tool calls", func(t *testing.T) {
		one := `{"message":{"role":"assistant","tool_calls":[{"function":{"name":"view_slice","arguments":{}}}]}}` + "\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < maxStreamCalls+10; i++ {
				if _, err := io.WriteString(w, one); err != nil {
					return
				}
			}
		}))
		defer srv.Close()
		opts := llm.DefaultOptions()
		opts.Stream = true
		_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "tool calls") {
			t.Fatalf("Chat() error = %v, want the call count bounded", err)
		}
	})
}
