package ollama

import (
	"context"
	"strings"
	"testing"
)

// TestValidateEndpointAcceptsHTTPChatEndpoint: the configurations metron is built
// around -- a local Ollama over http(s) -- must pass the guard.
func TestValidateEndpointAcceptsHTTPChatEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:11434/api/chat",
		"https://127.0.0.1:11434/api/chat",
		"http://10.0.0.5:11434/api/chat",
	} {
		if err := validateEndpoint(endpoint); err != nil {
			t.Errorf("validateEndpoint(%q) = %v, want it accepted", endpoint, err)
		}
	}
}

// TestValidateEndpointRefusesForeignSchemes is the point of the guard: a project
// config is attacker-influenceable, so a scheme that is not http(s) -- file, ftp,
// gopher -- must never be turned into the exfiltration channel the guard exists
// to close. An endpoint that names no host is refused the same way, since a
// request with no host reaches no local model at all.
func TestValidateEndpointRefusesForeignSchemes(t *testing.T) {
	for _, endpoint := range []string{
		"file:///etc/passwd",
		"ftp://example.invalid/api/chat",
		"gopher://169.254.169.254",
		"http://",
	} {
		if err := validateEndpoint(endpoint); err == nil {
			t.Errorf("validateEndpoint(%q) = nil, want it refused", endpoint)
		}
	}
}

// TestValidateEndpointLeavesAUnparseableEndpointToTheTransport pins the deferral:
// a string that is not a URL reaches no host, so it is not exfiltration -- New
// Request turns it into a "create request" error instead.
func TestValidateEndpointLeavesAUnparseableEndpointToTheTransport(t *testing.T) {
	if err := validateEndpoint("ht!tp://x"); err != nil {
		t.Fatalf("validateEndpoint(%q) = %v, want it deferred to request construction", "ht!tp://x", err)
	}
	_, err := NewClient("ht!tp://x", "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create request") {
		t.Fatalf("Chat() error = %v, want a request-construction error", err)
	}
}

func TestChatRefusesAForeignSchemeEndpoint(t *testing.T) {
	// A "file://" endpoint must not be turned into an HTTP POST to a host the
	// operator never chose. The refusal happens before any request is built.
	_, err := NewClient("file:///etc/passwd", "m", DefaultOptions()).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid Ollama chat endpoint") {
		t.Fatalf("Chat() error = %v, want the foreign scheme refused up front", err)
	}
}

func TestLoopbackEndpoint(t *testing.T) {
	loop := []string{
		"http://127.0.0.1:11434/api/chat",
		"https://localhost:11434/api/chat",
		"http://[::1]:11434/api/chat",
		"http://localhost/api/chat",
	}
	for _, endpoint := range loop {
		_, ok := loopback(endpoint)
		if !ok {
			t.Errorf("loopback(%q) = false, want true", endpoint)
		}
	}

	remote := []string{
		"http://10.0.0.5:11434/api/chat",
		"https://example.invalid:11434/api/chat",
		"http://169.254.169.254/latest/meta-data",
	}
	for _, endpoint := range remote {
		_, ok := loopback(endpoint)
		if ok {
			t.Errorf("loopback(%q) = true, want false", endpoint)
		}
	}

	// An unparseable endpoint is not loopback, and yields no URL to reuse.
	if u, ok := loopback(":://not a url"); ok || u != nil {
		t.Errorf("loopback(%q) = (%v, %v), want (nil, false)", ":://not a url", u, ok)
	}
}

// TestEndpointWarningSilentForLoopback is why the default configuration stays
// quiet: a local model over a local transport is not a thing to warn about, and
// a warning on every run would train the operator to ignore it.
func TestEndpointWarningSilentForLoopback(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:11434/api/chat",
		"https://127.0.0.1:11434/api/chat",
	} {
		if w := EndpointWarning(endpoint); w != "" {
			t.Errorf("EndpointWarning(%q) = %q, want no warning", endpoint, w)
		}
	}
}

func TestEndpointWarningNamesTheExfiltrationSurface(t *testing.T) {
	w := EndpointWarning("http://169.254.169.254/latest/meta-data/attack")
	if !strings.Contains(w, "169.254.169.254") {
		t.Errorf("EndpointWarning() = %q, want it to name the host", w)
	}
	if !strings.Contains(w, "cleartext") {
		t.Errorf("EndpointWarning() over http = %q, want it to flag the cleartext transport", w)
	}
}

// TestEndpointWarningNotesCleartextForRemoteHTTP but not HTTPS pins the part of
// the message that survives even when the host is otherwise trusted: an attacker
// on the path can read http, not https.
func TestEndpointWarningFlagsCleartextOnlyForRemoteHTTP(t *testing.T) {
	http := EndpointWarning("http://10.0.0.5:11434/api/chat")
	if !strings.Contains(http, "cleartext") {
		t.Errorf("remote http = %q, want the cleartext note", http)
	}

	https := EndpointWarning("https://10.0.0.5:11434/api/chat")
	if strings.Contains(https, "cleartext") {
		t.Errorf("remote https = %q, want no cleartext note", https)
	}
	if !strings.Contains(https, "10.0.0.5") {
		t.Errorf("remote https = %q, want the exfiltration warning still present", https)
	}
}

// TestProbeRequiresTheChatPath pins the one check Probe keeps that Chat drops:
// an endpoint that does not name /api/chat cannot be rewritten to /api/show, so
// the refusal must come from showEndpoint, not from a later dial error.
func TestProbeRequiresTheChatPath(t *testing.T) {
	_, err := NewClient("https://127.0.0.1:11434/api/bad", "m", DefaultOptions()).Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid Ollama chat endpoint") {
		t.Fatalf("Probe() error = %v, want the missing /api/chat path refused", err)
	}
}

// TestProbeRefusesAForeignScheme is the exfiltration check where the doctor probe
// runs: a non-http(s) endpoint must fail the probe rather than reach the host.
// The path carries /api/chat on purpose, so the refusal comes from the scheme
// guard (endpointAllowed) and not the earlier suffix check.
func TestProbeRefusesAForeignScheme(t *testing.T) {
	_, err := NewClient("ftp://169.254.169.254/api/chat", "m", DefaultOptions()).Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid Ollama chat endpoint") {
		t.Fatalf("Probe() error = %v, want the foreign scheme refused", err)
	}
}

// TestProbeRefusesAnUnparseableEndpoint exercises showEndpoint's parse-error
// return: a value that is not a URL fails the probe before anything is built.
func TestProbeRefusesAnUnparseableEndpoint(t *testing.T) {
	_, err := NewClient(":://not a url", "m", DefaultOptions()).Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid Ollama endpoint") {
		t.Fatalf("Probe() error = %v, want the unparseable endpoint refused", err)
	}
}
