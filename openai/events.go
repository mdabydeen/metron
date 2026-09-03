package openai

import (
	"bufio"
	"io"
	"strings"
)

// maxEventLine bounds one server-sent event. A single chunk carrying a large
// tool-call fragment exceeds bufio.Scanner's 64KB default, which would end the
// stream mid-reply and look like a truncated answer.
const maxEventLine = 4 << 20

// eventScanner reads a server-sent-events stream, yielding the payload of each
// `data:` line. Everything else -- comments, `event:` lines, blank separators --
// is skipped, because the chat-completions stream carries nothing else metron
// needs and tolerating them costs nothing.
type eventScanner struct {
	scanner *bufio.Scanner
}

func newEventScanner(r io.Reader) *eventScanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxEventLine)
	return &eventScanner{scanner: s}
}

// next returns the next event payload, and whether there was one.
func (e *eventScanner) next() (string, bool) {
	for e.scanner.Scan() {
		line := strings.TrimSpace(e.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		return strings.TrimSpace(payload), true
	}
	return "", false
}

func (e *eventScanner) err() error { return e.scanner.Err() }
