// Package proxy forwards client requests to the OpenCode Go upstream, applying
// key selection, model-id rewriting, streaming pass-through, and quota capture.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/user/go2api/internal/keypool"
)

// Upstream is the configured base URL of the OpenCode Go API.
type Upstream struct {
	BaseURL string
	Timeout time.Duration
	Client  *http.Client
}

// New builds an Upstream with a sane HTTP client (no automatic retries; failover
// is handled by the keypool).
func New(baseURL string, timeout time.Duration) *Upstream {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Upstream{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: timeout,
		Client:  &http.Client{Timeout: timeout, Transport: t},
	}
}

// ForwardPlan describes a single upstream attempt: which path, which body, which auth header.
type ForwardPlan struct {
	Endpoint string // e.g. "/chat/completions" or "/messages"
	Method   string
	Body     []byte
	Model    string // client-facing model id
	Stream   bool
}

// Result is what the upstream returned (or a partial of it for streaming).
type Result struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
	Streaming   bool
}

// ErrUpstream is returned when the upstream returned a non-2xx status code.
type ErrUpstream struct {
	Status int
	Body   []byte
}

func (e *ErrUpstream) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, truncate(string(e.Body), 256))
}

// Proxy ties an Upstream to a keypool.
type Proxy struct {
	upstream *Upstream
	pool     *keypool.Pool
	logger   *slog.Logger
}

// NewProxy builds a Proxy.
func NewProxy(u *Upstream, pool *keypool.Pool, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{upstream: u, pool: pool, logger: logger}
}

// Pool returns the underlying key pool. Useful for handlers that want to
// reuse scheduling, failover, and quota accounting logic outside of Forward.
func (p *Proxy) Pool() *keypool.Pool { return p.pool }

// BaseURL returns the upstream base URL.
func (p *Proxy) BaseURL() string { return p.upstream.BaseURL }

// Client returns the HTTP client used for upstream calls.
func (p *Proxy) Client() *http.Client { return p.upstream.Client }

// PickKey is a thin wrapper around pool.Pick for use by handlers that perform
// their own request loop (e.g. format-converting handlers).
func (p *Proxy) PickKey(ctx context.Context) (*keypool.Key, error) {
	return p.pool.Pick(ctx)
}

// MarkSuccess records a successful call for the given key.
func (p *Proxy) MarkSuccess(ctx context.Context, k *keypool.Key) {
	p.pool.MarkSuccess(ctx, k)
}

// MarkFailure records a failed call.
func (p *Proxy) MarkFailure(ctx context.Context, k *keypool.Key, status int, errStr string) {
	p.pool.MarkFailure(ctx, k, status, errStr)
}

// Forward executes the request, picking keys and applying failover per the pool config.
// It returns the final upstream response (or error). If streaming, it also streams into w.
func (p *Proxy) Forward(ctx context.Context, plan ForwardPlan, w http.ResponseWriter) (*Result, error) {
	maxAttempts := 1
	if p.pool.Enabled() {
		maxAttempts = 1 + p.pool.MaxRetries()
	}
	if maxAttempts > 10 {
		maxAttempts = 10
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		key, err := p.pool.Pick(ctx)
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			p.logger.Info("retrying with next key", "attempt", attempt+1, "key", key.ID)
		}
		result, err := p.callOnce(ctx, plan, key, w)
		if err == nil {
			p.pool.MarkSuccess(ctx, key)
			p.pool.RecordQuota(ctx, key, result.Headers)
			return result, nil
		}
		lastErr = err
		var ue *ErrUpstream
		status := 0
		if errors.As(err, &ue) {
			status = ue.Status
		}
		errStr := err.Error()
		if ue != nil {
			errStr = ue.Error()
		}
		retryable := p.pool.IsRetryableStatus(status)
		p.pool.MarkFailure(ctx, key, status, errStr)
		if !retryable || !p.pool.Enabled() {
			return nil, err
		}
	}
	return nil, fmt.Errorf("proxy: exhausted %d attempts: %w", maxAttempts, lastErr)
}

// callOnce executes a single upstream request for the given key.
// For non-streaming requests, the body is buffered into Result.Body.
// For streaming requests, the body is forwarded to w chunk-by-chunk and Result.Body is nil.
func (p *Proxy) callOnce(ctx context.Context, plan ForwardPlan, key *keypool.Key, w http.ResponseWriter) (*Result, error) {
	url := p.upstream.BaseURL + plan.Endpoint
	body, err := rewriteModel(plan.Body, plan.Model)
	if err != nil {
		return nil, fmt.Errorf("rewrite model: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, plan.Method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key.APIKey)
	// The /messages endpoint (Anthropic-compatible) requires the key in the
	// x-api-key header, not just Authorization. Sending both is safe and
	// lets the same key work for both endpoint families.
	if plan.Endpoint == "/messages" {
		req.Header.Set("x-api-key", key.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := p.upstream.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream do: %w", err)
	}
	defer resp.Body.Close()

	headers := flattenHeaders(resp.Header)

	if resp.StatusCode >= 400 {
		// Drain body for the error so we can return it.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, &ErrUpstream{Status: resp.StatusCode, Body: b}
	}

	if plan.Stream && w != nil {
		if err := streamResponse(w, resp); err != nil {
			return nil, fmt.Errorf("stream: %w", err)
		}
		return &Result{
			Status:      resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Headers:     headers,
			Streaming:   true,
		}, nil
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return &Result{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        bodyBytes,
		Headers:     headers,
		Streaming:   false,
	}, nil
}

// rewriteModel normalizes the model field in the request body. The upstream
// OpenCode Go HTTP API expects bare model IDs (e.g. "minimax-m3", "kimi-k3"),
// NOT the "opencode-go/<id>" form — that prefix is only used in OpenCode's
// client configuration file, not in API request bodies. If the client already
// sent a prefixed model, we strip the prefix so the upstream accepts it.
func rewriteModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body, nil
	}
	v["model"] = strings.TrimPrefix(model, "opencode-go/")
	return json.Marshal(v)
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

// streamResponse copies SSE chunks from the upstream into the client response,
// flushing after every chunk. It also writes the response headers first.
func streamResponse(w http.ResponseWriter, resp *http.Response) error {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 16*1024)
	buf := make([]byte, 16*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
