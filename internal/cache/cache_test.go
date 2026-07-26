package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKeyFor_DeterministicAcrossKeyOrder(t *testing.T) {
	body1 := `{"model":"kimi-k3","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	body2 := `{"messages":[{"content":"hi","role":"user"}],"temperature":0.7,"model":"kimi-k3"}`

	h1, err := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(body1)})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(body2)})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("expected equal hashes, got %s vs %s", h1, h2)
	}
}

func TestKeyFor_DifferentContentProducesDifferentKeys(t *testing.T) {
	body1 := `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`
	body2 := `{"model":"kimi-k3","messages":[{"role":"user","content":"bye"}]}`

	h1, _ := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(body1)})
	h2, _ := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(body2)})

	if h1 == h2 {
		t.Fatalf("expected different hashes for different content")
	}
}

func TestKeyFor_StreamRejected(t *testing.T) {
	body := `{"model":"kimi-k3","stream":true,"messages":[]}`
	_, err := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", Stream: true, RawBody: json.RawMessage(body)})
	if err == nil || !strings.Contains(err.Error(), "streaming") {
		t.Fatalf("expected streaming rejection, got %v", err)
	}
}

func TestKeyFor_StreamFlagIgnoredWhenFalse(t *testing.T) {
	a := `{"model":"kimi-k3","stream":false,"messages":[]}`
	b := `{"model":"kimi-k3","messages":[]}`
	h1, _ := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(a)})
	h2, _ := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "kimi-k3", RawBody: json.RawMessage(b)})
	if h1 != h2 {
		t.Fatalf("stream flag should be stripped from cache key")
	}
}

func TestKeyFor_DifferentEndpointsProduceDifferentKeys(t *testing.T) {
	body := `{"model":"x","messages":[]}`
	h1, _ := KeyFor(Fingerprint{Endpoint: "/v1/chat/completions", Model: "x", RawBody: json.RawMessage(body)})
	h2, _ := KeyFor(Fingerprint{Endpoint: "/v1/messages", Model: "x", RawBody: json.RawMessage(body)})
	if h1 == h2 {
		t.Fatalf("expected different hashes for different endpoints")
	}
}
