package responses

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// SSEMessage is one parsed upstream SSE event. Event is the optional `event:`
// field; Data is the joined payload of one or more `data:` lines (without the
// trailing blank-line separator that delivered them).
type SSEMessage struct {
	Event string
	Data  []byte
}

// ReadSSE consumes the next complete upstream event from r. It returns
// (msg, nil) on success, or (zero, io.EOF) when the upstream closes cleanly.
//
// Compatible wire rules covered:
//
//   - LF and CRLF line endings (we strip trailing \r\n).
//   - Multiple `data:` lines per event are joined with \n, per the SSE spec.
//   - Comments (`:` prefix) and empty fields are ignored per spec.
//   - An event terminating without a blank line at EOF is still emitted so
//     providers that omit the trailing separator (some implementations of
//     "messages" endpoint do) don't drop their last frame.
//   - `event:` defaults to "" if absent. The "messages" upstream sends
//     named events ("message_start", "content_block_delta", ...); OpenAI
//     Chat Completions leave event blank and uses `data:` only.
func ReadSSE(r *bufio.Reader) (SSEMessage, error) {
	var (
		eventName []byte
		dataLines [][]byte
	)
	for {
		raw, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return SSEMessage{}, err
		}
		// Strip trailing \n and the carriage return before it. Some Upstreams
		// (notably Anthropic via certain proxies) emit \r\n terminators.
		line := bytes.TrimRight(raw, "\n")
		line = bytes.TrimRight(line, "\r")

		if len(line) == 0 {
			if len(dataLines) == 0 && len(eventName) == 0 {
				// Pure blank line at the start, or double-blank separator.
				// If we hit EOF while looking, fall through to the EOF
				// branch below.
				if err == io.EOF {
					return SSEMessage{}, io.EOF
				}
				continue
			}
			return SSEMessage{
				Event: string(eventName),
				Data:  bytes.Join(dataLines, []byte("\n")),
			}, nil
		}

		// Comment / keep-alive — discard.
		if line[0] == ':' {
			if err == io.EOF {
				return SSEMessage{}, io.EOF
			}
			continue
		}

		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			if err == io.EOF {
				return SSEMessage{}, io.EOF
			}
			continue
		}
		field := string(line[:colon])
		value := line[colon+1:]
		// SSE allows a single leading space after the colon; strip it.
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}

		switch field {
		case "event":
			eventName = append(eventName[:0], value...)
		case "data":
			dataLines = append(dataLines, append([]byte{}, value...))
		case "id", "retry":
			// Unsupported in this scope; ignore.
		}

		if err == io.EOF {
			if len(dataLines) == 0 && len(eventName) == 0 {
				return SSEMessage{}, io.EOF
			}
			// Drain a final event at EOF even without a trailing blank line.
			return SSEMessage{
				Event: string(eventName),
				Data:  bytes.Join(dataLines, []byte("\n")),
			}, io.EOF
		}
	}
}

// ErrEmptyPayload is returned when an upstream SSE chunk has no `data:` body
// (e.g. keepalives, comment-only events, or pure pings). The handler should
// treat this as a no-op rather than a stream error.
var ErrEmptyPayload = errors.New("responses: empty SSE payload")

// IsDonePayload reports whether a `data:` payload is the upstream [DONE]
// sentinel. Chat Completions uses this exact string.
func IsDonePayload(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
}

// FormatFieldForLog is a debug helper used by tests.
func FormatFieldForLog(s string) string { return strings.TrimSpace(s) }
