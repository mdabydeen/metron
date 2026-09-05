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
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("http://example.invalid/api/chat", "test-model", DefaultOptions())

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
	for key, want := range map[string]float64{"temperature": 0.1, "top_p": 0.95, "num_ctx": 16384, "num_predict": 4096} {
		if got, ok := gotBody.Options[key].(float64); !ok || got != want {
			t.Errorf("options[%q] = %v, want %v", key, gotBody.Options[key], want)
		}
	}
}

func TestProbeChecksConfiguredModelCapabilities(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotModel = body["model"]
		_, _ = io.WriteString(w, `{"capabilities":["completion","tools"]}`)
	}))
	defer srv.Close()

	info, err := NewClient(srv.URL+"/api/chat", "qwen", DefaultOptions()).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if gotPath != "/api/show" || gotModel != "qwen" || !info.Supports("tools") || info.Supports("vision") {
		t.Fatalf("Probe() = %+v, request path %q model %q", info, gotPath, gotModel)
	}
}

func TestProbeReportsEndpointHTTPAndDecodeFailures(t *testing.T) {
	t.Run("invalid endpoint", func(t *testing.T) {
		_, err := NewClient("not-an-ollama-url", "m", DefaultOptions()).Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "invalid Ollama chat endpoint") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
	t.Run("malformed URL", func(t *testing.T) {
		_, err := NewClient("http://[::1/api/chat", "m", DefaultOptions()).Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "invalid Ollama endpoint") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		_, err := NewClient("http://127.0.0.1:1/api/chat", "m", DefaultOptions()).Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "contact Ollama") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
	t.Run("nil context", func(t *testing.T) {
		// Passing nil is deliberate: it is the only way to exercise and retain
		// Probe's defensive request-construction error path.
		_, err := NewClient("http://localhost/api/chat", "m", DefaultOptions()).Probe(nil) //nolint:staticcheck
		if err == nil || !strings.Contains(err.Error(), "create model probe") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model missing", http.StatusNotFound)
		}))
		defer srv.Close()
		_, err := NewClient(srv.URL+"/api/chat", "m", DefaultOptions()).Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "model missing") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
	t.Run("bad response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "not json")
		}))
		defer srv.Close()
		_, err := NewClient(srv.URL+"/api/chat", "m", DefaultOptions()).Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "decode Ollama model details") {
			t.Fatalf("Probe() error = %v", err)
		}
	})
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
	// An endpoint that is not parseable is not exfiltration -- it reaches no host
	// -- so the guard defers it and the request layer reports it.
	_, err := NewClient(":://not a url", "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create request") {
		t.Fatalf("Chat() error = %v, want a request-construction error", err)
	}
}

func TestNewClientFallsBackToTheDefaultTimeout(t *testing.T) {
	c := NewClient("http://example.invalid", "m", Options{Temperature: 0.2})

	if c.opts.Timeout != DefaultOptions().Timeout {
		t.Fatalf("timeout = %v, want the default %v", c.opts.Timeout, DefaultOptions().Timeout)
	}
}

func TestChatSendsConfiguredSamplingOptions(t *testing.T) {
	var got ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(ChatResponse{Message: Message{Role: "assistant"}})
	}))
	defer srv.Close()

	opts := Options{Temperature: 0.7, TopP: 0.5, NumCtx: 4096, MaxOutputTokens: 512, Timeout: 5 * time.Second}
	if _, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	for key, want := range map[string]float64{"temperature": 0.7, "top_p": 0.5, "num_ctx": 4096, "num_predict": 512} {
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

	got, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
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

	got, err := NewClient(srv.URL, "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if (got.Usage != Usage{}) {
		t.Fatalf("Usage = %+v, want zero when the server reports nothing", got.Usage)
	}
}

func TestUsageAddAccumulates(t *testing.T) {
	u := Usage{PromptTokens: 10, GenTokens: 2}
	u.Add(Usage{PromptTokens: 5, GenTokens: 3})

	if (u != Usage{PromptTokens: 15, GenTokens: 5}) {
		t.Fatalf("Usage = %+v, want the counts summed", u)
	}
}

// streamOpts requests streaming with content echoed to sink.
func streamOpts(sink io.Writer) Options {
	o := DefaultOptions()
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

	opts := DefaultOptions()
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

	opts := DefaultOptions()
	opts.Timeout = 50 * time.Millisecond
	_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)

	if err == nil {
		t.Fatal("Chat() = nil error, want the idle watchdog to give up")
	}
}
