package updates

import "strings"

const DiagnosticOutputLimit = 256 * 1024
const ParsedOutputLimit = 4 * 1024 * 1024
const LiveStatusLogLimit = 32 * 1024
const OutputTruncationMarker = "\n[output truncated]\n"

// OutputBuffer drains arbitrarily large output while retaining a bounded head
// and circular tail. Callers that parse complete output must check Truncated.
// Synchronization belongs to the owning writer.
type OutputBuffer struct {
	Limit      int
	head, tail []byte
	next       int
	Truncated  bool
}

func (b *OutputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.Limit
	if limit <= 0 {
		limit = DiagnosticOutputLimit
	}
	headLimit := limit / 2
	if len(b.head) < headLimit {
		count := min(len(p), headLimit-len(b.head))
		b.head = append(b.head, p[:count]...)
		p = p[count:]
	}
	tailLimit := limit - headLimit
	if len(b.tail) < tailLimit {
		count := min(len(p), tailLimit-len(b.tail))
		b.tail = append(b.tail, p[:count]...)
		p = p[count:]
	}
	if len(p) > 0 {
		b.Truncated = true
		if len(p) >= tailLimit {
			p = p[len(p)-tailLimit:]
		}
		count := copy(b.tail[b.next:], p)
		copy(b.tail, p[count:])
		b.next = (b.next + len(p)) % tailLimit
	}
	return n, nil
}

func (b *OutputBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

func (b *OutputBuffer) String() string {
	var out strings.Builder
	out.Grow(len(b.head) + len(b.tail) + len(OutputTruncationMarker))
	out.Write(b.head)
	if b.Truncated {
		out.WriteString(OutputTruncationMarker)
	}
	out.Write(b.tail[b.next:])
	out.Write(b.tail[:b.next])
	return out.String()
}

func BoundStatusLogs(value string) string {
	if len(value) <= LiveStatusLogLimit {
		return value
	}
	return OutputTruncationMarker + value[len(value)-LiveStatusLogLimit+len(OutputTruncationMarker):]
}
