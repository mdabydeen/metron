package ollama

import (
	"fmt"
	"net"
	"net/url"
)

// endpointAllowed is the single policy that decides whether a parsed Ollama
// endpoint is acceptable: an http(s) scheme and a non-empty host, and nothing
// else. Every path that turns a configured endpoint into a request funnels
// through it, so the exfiltration surface a project config could otherwise carry
// -- file://, ftp://, gopher://, or an endpoint with no host -- is closed in one
// place. The original string is passed so the refusal names what was configured.
func endpointAllowed(u *url.URL, endpoint string) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid Ollama chat endpoint %q (want http(s)://host/api/chat)", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid Ollama chat endpoint %q (want http(s)://host/api/chat)", endpoint)
	}
	return nil
}

// validateEndpoint is the guard Chat runs before building a request. A string that
// cannot be parsed is not exfiltration -- it reaches no host -- so that case is
// left to New Request, which already turns it into a "create request" error; here
// only the parseable-but-misrouted cases are refused.
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	return endpointAllowed(u, endpoint)
}

// loopback reports whether an endpoint's host is the loopback interface and hands
// back the parsed URL so a caller need not parse it twice. The local Ollama model
// that metron is built around keeps the whole conversation on the machine; a host
// that is not loopback is where that property stops holding. An endpoint whose
// host cannot be named -- an unparseable value -- is not loopback.
func loopback(endpoint string) (*url.URL, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false
	}
	host := u.Hostname()
	if host == "localhost" {
		return u, true
	}
	ip := net.ParseIP(host)
	return u, ip != nil && ip.IsLoopback()
}

// EndpointWarning names the data-exfiltration surface of a non-local endpoint.
// It is empty when the model is reached over loopback -- the configuration the
// program is built around, where the conversation never leaves the machine and a
// cleartext transport has no wire an attacker can sit on. A host that is not
// loopback is where that property stops holding, and that is worth saying out
// loud: it concerns the operator, not the model.
func EndpointWarning(endpoint string) string {
	u, local := loopback(endpoint)
	if local {
		return ""
	}
	transport := "leaves this machine"
	if u != nil && u.Scheme == "http" {
		transport = "leaves this machine over cleartext http"
	}
	return fmt.Sprintf("endpoint %q is not a loopback Ollama: every request, with its full conversation, "+
		"%s -- review it before trusting the project", endpoint, transport)
}
