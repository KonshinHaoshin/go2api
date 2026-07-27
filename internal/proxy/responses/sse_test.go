package responses

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSE_BasicEvent(t *testing.T) {
	src := "data: hello\n\n"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if msg.Event != "" {
		t.Fatalf("expected empty event, got %q", msg.Event)
	}
	if string(msg.Data) != "hello" {
		t.Fatalf("data mismatch: %q", msg.Data)
	}
}

func TestReadSSE_CRLF(t *testing.T) {
	src := "data: hello\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(msg.Data) != "hello" {
		t.Fatalf("data mismatch: %q", msg.Data)
	}
}

func TestReadSSE_NamedEvent(t *testing.T) {
	src := "event: message_start\ndata: {\"x\":1}\n\n"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if msg.Event != "message_start" {
		t.Fatalf("event mismatch: %q", msg.Event)
	}
	if string(msg.Data) != `{"x":1}` {
		t.Fatalf("data mismatch: %q", msg.Data)
	}
}

func TestReadSSE_MultilineData(t *testing.T) {
	src := "data: line1\ndata: line2\n\n"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(msg.Data) != "line1\nline2" {
		t.Fatalf("multiline data mismatch: %q", msg.Data)
	}
}

func TestReadSSE_CommentIgnored(t *testing.T) {
	// Comment line should be silently skipped; the data: line that follows
	// must reach the caller.
	src := ": ping\ndata: hello\n\n"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(msg.Data) != "hello" {
		t.Fatalf("expected hello, got %q", msg.Data)
	}
}

func TestReadSSE_EOFWithoutBlankLine(t *testing.T) {
	src := "data: unterminated"
	r := bufio.NewReader(strings.NewReader(src))
	msg, err := ReadSSE(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
	if string(msg.Data) != "unterminated" {
		t.Fatalf("expected data to surface on EOF, got %q", msg.Data)
	}
	_ = msg
}

func TestReadSSE_KeepAliveAloneReturnsEOF(t *testing.T) {
	// Pure keep-alive (blank lines / comments) followed by EOF.
	src := ": hi\n\n"
	r := bufio.NewReader(strings.NewReader(src))
	_, err := ReadSSE(r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF for empty event stream, got %v", err)
	}
}

func TestIsDonePayload(t *testing.T) {
	if !IsDonePayload([]byte("[DONE]")) {
		t.Fatal("expected [DONE]")
	}
	if !IsDonePayload([]byte(" [DONE]\n")) {
		t.Fatal("expected trimmed [DONE]")
	}
	if IsDonePayload([]byte("data")) {
		t.Fatal("expected non-match")
	}
}
