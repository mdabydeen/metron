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

	"github.com/mdabydeen/metron/llm"
)

// serve runs a stub endpoint that hands back the given body, and records the
// request it was sent.
func serve(t *testing.T, status int, body string) (*Client, *chatRequest, llm.Options) {
	t.Helper()
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	opts := llm.DefaultOptions()
	return NewClient(srv.URL, "m", opts), &got, opts
}

func TestChatReturnsAnAnswer(t *testing.T) {
	c, got, _ := serve(t, 200, `{"choices":[{"message":{"role":"assistant","content":"hello"}}],
		"usage":{"prompt_tokens":120,"completion_tokens":8}}`)

	reply, err := c.Chat(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if reply.Content != "hello" {
		t.Fatalf("content = %q", reply.Content)
	}
	// Usage keys differ from Ollama's; metron's whole premise is measuring it.
	if reply.Usage.PromptTokens != 120 || reply.Usage.GenTokens != 8 {
		t.Fatalf("usage = %+v, want the OpenAI counts mapped", reply.Usage)
	}
	if got.Model != "m" || len(got.Messages) != 1 {
		t.Fatalf("request = %+v", got)
	}
}

func TestChatParsesToolCallArgumentsFromAString(t *testing.T) {
	// Arguments arrive as a JSON *string*, not as JSON.
	c, _, _ := serve(t, 200, `{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"1","type":"function","function":{"name":"view_slice",
		 "arguments":"{\"path\":\"a.go\",\"start\":1,\"end\":5}"}}]}}]}`)

	reply, err := c.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", reply.ToolCalls)
	}
	call := reply.ToolCalls[0]
	if call.Function.Name != "view_slice" || call.Function.Arguments["path"] != "a.go" {
		t.Fatalf("call = %+v, want the arguments string parsed into an object", call)
	}
}

func TestChatReportsUnparseableArguments(t *testing.T) {
	c, _, _ := serve(t, 200, `{"choices":[{"message":{"tool_calls":[
		{"function":{"name":"view_slice","arguments":"{not json"}}]}}]}`)

	if _, err := c.Chat(context.Background(), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "view_slice") {
		t.Fatalf("Chat() error = %v, want the offending call named", err)
	}
}

// TestStreamReassemblesFragmentedToolCalls is the difference that matters
// between this provider and Ollama's. Ollama sends whole tool calls; here the
// name arrives once and the arguments come as a JSON string split across any
// number of chunks, keyed by index. Reassembling by arrival order rather than
// by index turns two calls into one corrupt one.
func TestStreamReassemblesFragmentedToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"view_slice","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"list_files","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`,
		`data: {"usage":{"prompt_tokens":50,"completion_tokens":5}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	opts := llm.DefaultOptions()
	opts.Stream = true
	c := NewClient(srv.URL, "m", opts)

	reply, err := c.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(reply.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want two reassembled by index", reply.ToolCalls)
	}
	if got := reply.ToolCalls[0]; got.Function.Name != "view_slice" ||
		got.Function.Arguments["path"] != "a.go" {
		t.Fatalf("first call = %+v, want its split arguments joined", got)
	}
	if reply.ToolCalls[1].Function.Name != "list_files" {
		t.Fatalf("second call = %+v", reply.ToolCalls[1])
	}
	// stream_options.include_usage is why there are counts at all here.
	if reply.Usage.PromptTokens != 50 {
		t.Fatalf("usage = %+v, want the final chunk's counts", reply.Usage)
	}
}

func TestStreamEchoesContentToTheSink(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"one \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	var sink bytes.Buffer
	opts := llm.DefaultOptions()
	opts.Stream, opts.Sink = true, &sink

	reply, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if sink.String() != "one two" || reply.Content != "one two" {
		t.Fatalf("sink = %q, reply = %q, want both", sink.String(), reply.Content)
	}
}

func TestToolResultsCarryAToolCallID(t *testing.T) {
	c, got, _ := serve(t, 200, `{"choices":[{"message":{"content":"ok"}}]}`)

	_, err := c.Chat(context.Background(), []llm.Message{
		{Role: "tool", ToolName: "view_slice", Content: "    1 | x"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Without an id the server cannot pair a result with its call, which is the
	// pairing the whole tool contract depends on.
	if got.Messages[0].ToolCallID != "view_slice" {
		t.Fatalf("message = %+v, want the tool result identified", got.Messages[0])
	}
}

func TestAssistantToolCallsAreSentBackAsStrings(t *testing.T) {
	c, got, _ := serve(t, 200, `{"choices":[{"message":{"content":"ok"}}]}`)
	var call llm.ToolCall
	call.Function.Name = "view_slice"
	call.Function.Arguments = map[string]any{"path": "a.go"}

	if _, err := c.Chat(context.Background(), []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	sent := got.Messages[0].ToolCalls
	if len(sent) != 1 || !strings.Contains(sent[0].Function.Arguments, `"path"`) {
		t.Fatalf("sent = %+v, want arguments re-encoded as a JSON string", sent)
	}
}

func TestChatReportsTransportAndProtocolFailures(t *testing.T) {
	c, _, _ := serve(t, 500, "upstream exploded")
	if _, err := c.Chat(context.Background(), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Chat() error = %v, want the status surfaced", err)
	}

	c, _, _ = serve(t, 200, `{"error":{"message":"model not found"}}`)
	if _, err := c.Chat(context.Background(), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "model not found") {
		t.Fatalf("Chat() error = %v, want the API error surfaced", err)
	}

	c, _, _ = serve(t, 200, `{"choices":[]}`)
	if _, err := c.Chat(context.Background(), nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no choices") {
		t.Fatalf("Chat() error = %v, want the empty reply reported", err)
	}

	c, _, _ = serve(t, 200, `not json`)
	if _, err := c.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("Chat() = nil error for an undecodable body")
	}

	if _, err := NewClient("http://127.0.0.1:1/x", "m", llm.DefaultOptions()).
		Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("Chat() = nil error for an unreachable endpoint")
	}
	if _, err := NewClient("://bad", "m", llm.DefaultOptions()).
		Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("Chat() = nil error for an unparseable URL")
	}
}

func TestStreamReportsFailures(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"malformed chunk", "data: {not json}\n\n", "decode stream"},
		{"error chunk", `data: {"error":{"message":"boom"}}` + "\n\n", "boom"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, tc.body)
		}))
		opts := llm.DefaultOptions()
		opts.Stream = true
		_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestAPIKeyIsSentAsABearerToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	opts := llm.DefaultOptions()
	opts.APIKey = "sk-secret"

	if _, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-secret" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestNoAuthorizationHeaderWithoutAKey(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "m", llm.DefaultOptions()).Chat(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	// Ollama wants no key; sending an empty bearer is a needless difference.
	if present {
		t.Fatal("sent an Authorization header with no key configured")
	}
}

func TestNewClientAppliesADefaultTimeout(t *testing.T) {
	c := NewClient("http://x", "m", llm.Options{})
	if c.opts.Timeout != 180*time.Second {
		t.Fatalf("timeout = %v, want the default", c.opts.Timeout)
	}
	// The deadline lives on the idle watchdog, not the client: a client-level
	// one would cap the whole generation rather than the silence.
	if c.http.Timeout != 0 {
		t.Fatalf("http timeout = %v, want none", c.http.Timeout)
	}
}

func TestEventScannerSkipsNonDataLines(t *testing.T) {
	s := newEventScanner(strings.NewReader(
		": a comment\nevent: message\ndata: one\n\n\ndata: two\n"))

	for _, want := range []string{"one", "two"} {
		got, ok := s.next()
		if !ok || got != want {
			t.Fatalf("next() = %q, %v, want %q", got, ok, want)
		}
	}
	if _, ok := s.next(); ok {
		t.Fatal("next() returned a fourth event")
	}
	if err := s.err(); err != nil {
		t.Fatal(err)
	}
}

func TestFromWireCallsSkipsNamelessFragments(t *testing.T) {
	// A stream can carry an index with no name yet; it is not a call.
	got, err := fromWireCalls([]wireToolCall{{}})
	if err != nil || len(got) != 0 {
		t.Fatalf("fromWireCalls() = %v, %v, want it skipped", got, err)
	}
	if got, _ := fromWireCalls(nil); got != nil {
		t.Fatalf("fromWireCalls(nil) = %v, want nil", got)
	}
}

// failingReader errors part-way through, so the stream's read-failure path is
// reachable without a flaky network.
type failingReader struct{ done bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.ErrUnexpectedEOF
	}
	f.done = true
	n := copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	return n, nil
}

func TestStreamReportsAReadFailure(t *testing.T) {
	opts := llm.DefaultOptions()
	opts.Stream = true
	c := NewClient("http://x", "m", opts)

	if _, err := c.readStream(&failingReader{}, time.NewTimer(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "read stream") {
		t.Fatalf("readStream() error = %v, want the read failure reported", err)
	}
}

func TestStreamReportsUnparseableAssembledArguments(t *testing.T) {
	// The fragments concatenate into something that is not JSON, which is only
	// discoverable once the stream has finished.
	stream := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,` +
		`"function":{"name":"view_slice","arguments":"{oops"}}]}}]}` + "\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	opts := llm.DefaultOptions()
	opts.Stream = true

	_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "view_slice") {
		t.Fatalf("Chat() error = %v, want the offending call named", err)
	}
}

func TestStreamStopsAtDone(t *testing.T) {
	// Anything after [DONE] is not part of the reply.
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"kept\"}}]}\n\ndata: [DONE]\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" dropped\"}}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()
	opts := llm.DefaultOptions()
	opts.Stream = true

	reply, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "kept" {
		t.Fatalf("content = %q, want the stream to stop at [DONE]", reply.Content)
	}
}

func TestStreamRefusesAnUnboundedReply(t *testing.T) {
	// The idle watchdog resets on every chunk, so a server that streams steadily
	// is never idle and never cancelled. Without a size limit that is an
	// out-of-memory condition a remote endpoint can trigger at will.
	chunk := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 60000) + `"}}]}` + "\n\n"
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
}

func TestStreamRefusesTooManyToolCalls(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxStreamCalls+10; i++ {
		fmt.Fprintf(&b, `data: {"choices":[{"delta":{"tool_calls":[{"index":%d,`+
			`"function":{"name":"view_slice","arguments":"{}"}}]}}]}`+"\n\n", i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()
	opts := llm.DefaultOptions()
	opts.Stream = true

	// A server picks the index, so it is otherwise an unbounded map key.
	_, err := NewClient(srv.URL, "m", opts).Chat(context.Background(), nil, nil)

	if err == nil || !strings.Contains(err.Error(), "tool calls") {
		t.Fatalf("Chat() error = %v, want the call count bounded", err)
	}
}

func TestStreamRefusesUnboundedToolArguments(t *testing.T) {
	chunk := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":` +
		`{"name":"view_slice","arguments":"` + strings.Repeat("x", 60000) + `"}}]}}]}` + "\n\n"
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
		t.Fatalf("Chat() error = %v, want the arguments bounded", err)
	}
}
