// Package api is the thin client for the Servatum ingest API.
// C1 scope: one endpoint, one attempt. Retries, spooling and the second
// regional endpoint arrive in C2.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultEndpoint = "https://api.servatum.io"

// Status values accepted by the Run API.
const (
	StatusOK      = "ok"
	StatusWarn    = "warn"
	StatusFail    = "fail"
	StatusStarted = "started"
)

// MaxBody is the Run API body limit (16 KB per spec).
const MaxBody = 16 * 1024

// MaxPingBody is the ping body limit kept as the run's log (10 KB per spec).
const MaxPingBody = 10 * 1024

type Retention struct {
	Count  int    `json:"count,omitempty"`
	Oldest string `json:"oldest,omitempty"`
}

// Run is one report for POST /v1/runs. Only Job and Status are required.
type Run struct {
	Job            string     `json:"job"`
	Status         string     `json:"status"`
	StartedAt      string     `json:"started_at,omitempty"`
	FinishedAt     string     `json:"finished_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	SizeBytes      int64      `json:"size_bytes,omitempty"`
	SnapshotID     string     `json:"snapshot_id,omitempty"`
	Retention      *Retention `json:"retention,omitempty"`
	Message        string     `json:"message,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	// Kind names the integration that built Details, versioned
	// (e.g. "whm.backup.v1"). Both are optional; plain reports omit them.
	Kind    string          `json:"kind,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

type RunResponse struct {
	RunID string `json:"run_id"`
	State string `json:"state"`
}

type Client struct {
	Endpoint  string
	Key       string
	HTTP      *http.Client
	UserAgent string
}

func New(endpoint, key, version string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		Endpoint:  strings.TrimRight(endpoint, "/"),
		Key:       key,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		UserAgent: "servatum-cli/" + version,
	}
}

// APIError is a non-2xx answer from the API.
type APIError struct {
	Code       int
	Body       string
	RetryAfter string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("api returned %d", e.Code)
	if e.RetryAfter != "" {
		s += " (retry after " + e.RetryAfter + ")"
	}
	if b := strings.TrimSpace(e.Body); b != "" {
		s += ": " + b
	}
	return s
}

// Retryable reports whether C2's spool should keep the report and try again.
func (e *APIError) Retryable() bool {
	return e.Code == 429 || e.Code >= 500
}

// PostRun sends one run report. The message is trimmed (tail kept) so the
// body stays under MaxBody.
func (c *Client) PostRun(ctx context.Context, r Run) (*RunResponse, error) {
	if c.Key == "" {
		return nil, &ConfigError{"no ingest key: set SERVATUM_KEY or --key"}
	}
	if r.Job == "" {
		return nil, &ConfigError{"no job: set SERVATUM_JOB or --job"}
	}
	body, err := fitBody(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: resp.Header.Get("Retry-After")}
	}
	var out RunResponse
	_ = json.Unmarshal(raw, &out) // a 2xx with an odd body is still accepted
	return &out, nil
}

// Ping hits /p/<token>. status is "", "fail" or "start" (path suffix).
// A non-empty log is POSTed as the body (max MaxPingBody, tail kept).
func (c *Client) Ping(ctx context.Context, token, status, log string) error {
	if token == "" {
		return &ConfigError{"no ping token"}
	}
	url := c.Endpoint + "/p/" + token
	if status != "" {
		url += "/" + status
	}
	method, body := http.MethodGet, io.Reader(nil)
	if log != "" {
		method, body = http.MethodPost, strings.NewReader(tail(log, MaxPingBody))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if log != "" {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return &APIError{Code: resp.StatusCode, Body: string(raw), RetryAfter: resp.Header.Get("Retry-After")}
	}
	return nil
}

func fitBody(r Run) ([]byte, error) {
	for {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBody {
			return b, nil
		}
		over := len(b) - MaxBody
		if r.Message == "" || over >= len(r.Message) {
			r.Message = ""
			if b, err = json.Marshal(r); err == nil && len(b) <= MaxBody {
				return b, nil
			}
			return nil, fmt.Errorf("report exceeds %d bytes without a message", MaxBody)
		}
		// JSON escaping can grow the text, so trim a little extra each pass.
		r.Message = tail(r.Message, len(r.Message)-over-64)
	}
}

// tail keeps the last n bytes of s, cut on a UTF-8 boundary, with a marker.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…[truncated]\n"
	if n <= len(mark) {
		return ""
	}
	cut := len(s) - (n - len(mark))
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return mark + s[cut:]
}

// IsTransient reports whether err may clear up on its own (network trouble,
// 429, 5xx). A 4xx answer means the report itself or the config is wrong.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	var ce *ConfigError
	return !errors.As(err, &ce)
}

// ConfigError is a local problem (no key, no job): never retried or spooled.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// RetryDelays are the waits between inline attempts (~10 s worst case with
// the 15 s client timeout capped by the caller's context).
var RetryDelays = []time.Duration{1 * time.Second, 3 * time.Second}

// Retry runs fn up to len(RetryDelays)+1 times while the error is transient
// and ctx has time left.
func Retry(ctx context.Context, fn func(context.Context) error) error {
	err := fn(ctx)
	for _, d := range RetryDelays {
		if err == nil || !IsTransient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(d):
		}
		err = fn(ctx)
	}
	return err
}
