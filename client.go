package smtpmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is the SDK version, sent in the User-Agent header. It is bumped by
// release-please on every release.
const Version = "0.1.0" // x-release-please-version

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	// defaultMaxRetryAfter is the longest Retry-After the client waits out
	// before retrying; the server's send quota is per minute, so its
	// Retry-After goes up to 60s. See WithMaxRetryAfter.
	defaultMaxRetryAfter = 60 * time.Second
	// maxParsedRetryAfter bounds Error.RetryAfter whatever the header says,
	// so the value is always sane to sleep on.
	maxParsedRetryAfter = 24 * time.Hour
	// defaultRetryAfter is used when a 429 carries no Retry-After header.
	defaultRetryAfter = time.Second
	// serverErrorBackoff is the base delay between 5xx retries when enabled.
	serverErrorBackoff = 500 * time.Millisecond
	// maxSuccessBody bounds how much of a 2xx response is read; the message
	// resource is a few KiB at most.
	maxSuccessBody = 1024 * 1024
	// maxErrorBody bounds how much of an error response is read and kept in
	// Error.Body.
	maxErrorBody = 64 * 1024
	// maxDrain bounds how much of a body is discarded after the part we keep,
	// so the connection can be reused without reading a hostile body forever.
	maxDrain = 1024 * 1024

	// apiKeyPrefix is the one server rule mirrored locally: every key the
	// console issues starts with it, so a key without it is a configuration
	// mistake (wrong secret pasted) rather than user input to validate.
	apiKeyPrefix = "smtpk_"
	sendPath     = "/api/v1/messages"
	userAgent    = "smtp-manager-sdk-go/" + Version
)

// requestIDPattern is the shape of X-Request-Id the server keeps verbatim;
// anything else is replaced server-side, which would make the id useless for
// correlation.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Client sends messages through the SMTP Manager API. It is safe for
// concurrent use.
type Client struct {
	apiKey             string
	baseURL            string
	endpoint           string
	httpClient         *http.Client
	timeout            time.Duration
	maxRetries         int
	maxRetryAfter      time.Duration
	retryOnServerError bool
	userAgentSuffix    string

	// sleep waits for d or until ctx is done. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// New creates a Client for the given API key. WithBaseURL is required.
//
// API keys are shown once, at creation, in the SMTP Manager console next to
// the endpoint URL, and look like "smtpk_xxxxxxxxxx.<secret>". A key without
// the "smtpk_" prefix is rejected here as a configuration error.
func New(apiKey string, opts ...Option) (*Client, error) {
	c := &Client{
		apiKey:        apiKey,
		httpClient:    &http.Client{CheckRedirect: neverFollowRedirects},
		timeout:       defaultTimeout,
		maxRetries:    defaultMaxRetries,
		maxRetryAfter: defaultMaxRetryAfter,
		sleep:         sleepContext,
	}
	for _, opt := range opts {
		opt(c)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, &Error{Code: CodeConfiguration, Message: "api key is required"}
	}
	if !strings.HasPrefix(apiKey, apiKeyPrefix) {
		return nil, &Error{Code: CodeConfiguration, Message: `api key must start with "` + apiKeyPrefix + `"`}
	}
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, &Error{Code: CodeConfiguration, Message: "base URL is required (use WithBaseURL)"}
	}
	endpoint, err := resolveEndpoint(c.baseURL)
	if err != nil {
		return nil, err
	}
	c.endpoint = endpoint
	if strings.ContainsAny(c.userAgentSuffix, "\r\n") {
		return nil, &Error{Code: CodeConfiguration, Message: "user agent suffix must not contain CR or LF"}
	}
	return c, nil
}

// neverFollowRedirects makes http.Client hand back the 3xx itself: the send
// endpoint is never redirected, and following one would turn the POST into a
// GET or replay the body elsewhere.
func neverFollowRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// resolveEndpoint turns whatever the user pasted into the absolute send URL.
// The console's one-time key reveal shows the full endpoint
// ("https://host/api/v1/messages"); documentation and habit produce the bare
// origin. Both, and the partial prefixes in between, resolve to the same URL.
func resolveEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", &Error{Code: CodeConfiguration, Message: "base URL is not a valid URL", Cause: err}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", &Error{Code: CodeConfiguration, Message: fmt.Sprintf("base URL %q must be an absolute http(s) URL", raw)}
	}
	if u.User != nil {
		// net/http drops userinfo once Authorization is set, and it would
		// otherwise leak through Client.String().
		return "", &Error{Code: CodeConfiguration, Message: "base URL must not carry credentials; the API key is the only credential"}
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/api/v1/messages"):
		// already the endpoint
	case strings.HasSuffix(path, "/api/v1"):
		path += "/messages"
	case strings.HasSuffix(path, "/api"):
		path += "/v1/messages"
	default:
		path += sendPath
	}
	u.Path = path
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// Endpoint returns the absolute URL messages are posted to.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// UserAgent returns the User-Agent header value sent with every request.
func (c *Client) UserAgent() string {
	if c.userAgentSuffix == "" {
		return userAgent
	}
	return userAgent + " " + c.userAgentSuffix
}

// String describes the client with the API key redacted, so a Client can be
// logged or printed with %v without leaking the secret.
func (c *Client) String() string {
	return fmt.Sprintf("smtpmanager.Client{endpoint: %s, key: %s}", c.endpoint, redactKey(c.apiKey))
}

// GoString implements fmt.GoStringer with the same redaction as String, so
// %#v is safe too.
func (c *Client) GoString() string {
	return c.String()
}

// redactKey keeps the prefix and the first four characters of the key id and
// drops everything else. Nothing after the dot, where the secret lives, is
// ever echoed, however short the key.
func redactKey(key string) string {
	const keep = len(apiKeyPrefix) + 4
	if i := strings.IndexByte(key, '.'); i >= 0 && i < keep {
		return key[:i] + "…"
	}
	if len(key) <= keep {
		return key + "…"
	}
	return key[:keep] + "…"
}

// Send queues msg for delivery and returns the accepted message resource.
//
// Errors are *Error values. Rate limits (HTTP 429) and dial failures are
// retried up to the configured number of times; other failures are returned
// as-is. See WithRetryOnServerError before enabling 5xx retries.
func (c *Client) Send(ctx context.Context, msg *Message) (*SentMessage, error) {
	return c.SendWithRequestID(ctx, msg, "")
}

// SendWithRequestID is Send with an explicit X-Request-Id header, which the
// API echoes back and logs. Use it to correlate a send with your own
// tracing; leave it empty to let the server generate one. The id must match
// ^[A-Za-z0-9._-]{1,64}$, the shape the server keeps verbatim; anything else
// is a CodeInvalidInput error rather than an id the server would replace.
func (c *Client) SendWithRequestID(ctx context.Context, msg *Message, requestID string) (*SentMessage, error) {
	if requestID != "" && !requestIDPattern.MatchString(requestID) {
		return nil, &Error{
			Code:    CodeInvalidInput,
			Message: fmt.Sprintf("request id %q must match %s", requestID, requestIDPattern),
		}
	}
	wire, err := msg.toWire()
	if err != nil {
		return nil, err
	}
	body, err := encodeBody(wire)
	if err != nil {
		return nil, &Error{Code: CodeInvalidInput, Message: "encoding message", Cause: err}
	}

	var lastErr *Error
	for attempt := 0; ; attempt++ {
		sent, attemptErr := c.doAttempt(ctx, body, requestID)
		if attemptErr == nil {
			return sent, nil
		}
		lastErr = attemptErr
		if attempt >= c.maxRetries {
			break
		}
		delay, retry := c.retryDelay(attemptErr, attempt)
		if !retry {
			break
		}
		if err := c.sleep(ctx, delay); err != nil {
			// Keep what the last attempt learned: the caller can still see
			// the request id and the Retry-After the server asked for.
			e := contextError(err, lastErr)
			e.RequestID = lastErr.RequestID
			e.RetryAfter = lastErr.RetryAfter
			return nil, e
		}
	}
	return nil, lastErr
}

// encodeBody marshals the request without HTML-escaping, so the html field
// is sent verbatim rather than as < sequences.
func encodeBody(w *wireMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// retryDelay decides whether e is retryable and how long to wait first.
func (c *Client) retryDelay(e *Error, attempt int) (time.Duration, bool) {
	switch e.Code {
	case CodeRateLimited:
		d := e.RetryAfter
		if d <= 0 {
			d = defaultRetryAfter
		}
		if d > c.maxRetryAfter {
			// The limiter asked for longer than the caller is willing to
			// wait; surface the error now instead of retrying too early.
			return 0, false
		}
		return d, true
	case CodeNetwork:
		if e.retryable {
			return serverErrorBackoff * time.Duration(attempt+1), true
		}
	case CodeServer:
		if c.retryOnServerError {
			return serverErrorBackoff * time.Duration(attempt+1), true
		}
	}
	return 0, false
}

func (c *Client) doAttempt(ctx context.Context, body []byte, requestID string) (*SentMessage, *Error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Code: CodeInvalidInput, Message: "building request", Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent())
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() {
		// Drain what we did not keep so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Never followed (see neverFollowRedirects); a caller-supplied client
		// that does follow lands here only when the chain ends in a 3xx.
		msg := fmt.Sprintf("unexpected redirect (HTTP %d); check the base URL", resp.StatusCode)
		if loc := resp.Header.Get("Location"); loc != "" {
			msg = fmt.Sprintf("unexpected redirect (HTTP %d) to %s; check the base URL", resp.StatusCode, loc)
		}
		return nil, &Error{
			Code:       CodeUnexpectedResponse,
			StatusCode: resp.StatusCode,
			Message:    msg,
			RequestID:  resp.Header.Get("X-Request-Id"),
		}
	}

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	limit := int64(maxErrorBody)
	if success {
		limit = maxSuccessBody
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if !success {
		return nil, apiError(resp, respBody)
	}

	respRequestID := resp.Header.Get("X-Request-Id")
	var envelope struct {
		Data *wireSentMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, &Error{
			Code:       CodeUnexpectedResponse,
			StatusCode: resp.StatusCode,
			Message:    "response is not valid JSON; with a 2xx status the message was most likely queued, do not re-send",
			RequestID:  respRequestID,
			Body:       truncate(respBody, maxErrorBody),
			Cause:      err,
		}
	}
	if envelope.Data == nil {
		return nil, &Error{
			Code:       CodeUnexpectedResponse,
			StatusCode: resp.StatusCode,
			Message:    `response has no "data" object; with a 2xx status the message was most likely queued, do not re-send`,
			RequestID:  respRequestID,
			Body:       truncate(respBody, maxErrorBody),
		}
	}
	sent := envelope.Data.toSentMessage()
	sent.RequestID = respRequestID
	return sent, nil
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// apiError maps a non-2xx response to an *Error without assuming the body is JSON.
func apiError(resp *http.Response, body []byte) *Error {
	e := &Error{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
		Body:       truncate(body, maxErrorBody),
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.Code = CodeAuthentication
	case resp.StatusCode == http.StatusForbidden:
		e.Code = CodePermission
	case resp.StatusCode == http.StatusUnprocessableEntity:
		e.Code = CodeValidation
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Code = CodeRateLimited
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		e.Code = CodePayloadTooLarge
	case resp.StatusCode >= 500:
		e.Code = CodeServer
	default:
		e.Code = CodeUnexpectedResponse
	}

	var parsed struct {
		Message   string              `json:"message"`
		RequestID string              `json:"request_id"`
		Errors    map[string][]string `json:"errors"`
	}
	// A type mismatch on one field (say "errors" as a string) still leaves the
	// other fields decoded, so keep them rather than discarding the parse.
	var typeErr *json.UnmarshalTypeError
	if err := json.Unmarshal(body, &parsed); err == nil || errors.As(err, &typeErr) {
		e.Message = parsed.Message
		if parsed.RequestID != "" {
			e.RequestID = parsed.RequestID
		}
		if len(parsed.Errors) > 0 {
			e.Errors = parsed.Errors
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
		if e.Message == "" {
			e.Message = "unexpected HTTP status"
		}
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		e.RetryAfter = parseRetryAfter(ra, time.Now())
	}
	return e
}

// parseRetryAfter accepts the two forms RFC 9110 allows, delta-seconds
// (digits only) and an HTTP-date; anything else reads as absent. The result
// is never negative and never above maxParsedRetryAfter (24h), so it is safe
// to sleep on; whether the client actually waits is decided by
// WithMaxRetryAfter.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	var d time.Duration
	if isDigits(value) {
		secs, err := strconv.ParseUint(value, 10, 64)
		if err != nil || secs >= uint64(maxParsedRetryAfter/time.Second) {
			// Only ErrRange is possible for a digit string: a wait longer
			// than uint64 seconds is still "a very long wait".
			return maxParsedRetryAfter
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(value); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxParsedRetryAfter {
		return maxParsedRetryAfter
	}
	return d
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// transportError classifies an error from http.Client.Do or from reading the body.
func transportError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return contextError(ctx.Err(), err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "request timed out", Cause: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Code: CodeTimeout, Message: "request timed out", Cause: err}
	}
	e := &Error{Code: CodeNetwork, Message: "request failed", Cause: err}
	e.retryable = isDialError(err)
	return e
}

// contextError maps a done context to a timeout or network error. The
// transport error, when there is one, is kept as the cause; the context
// error stays reachable through errors.Is either way.
func contextError(ctxErr, transportErr error) *Error {
	cause := ctxErr
	switch {
	case transportErr == nil:
	case errors.Is(transportErr, ctxErr):
		cause = transportErr
	default:
		cause = errors.Join(ctxErr, transportErr)
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "request timed out", Cause: cause}
	}
	return &Error{Code: CodeNetwork, Message: "request cancelled", Cause: cause}
}

// isDialError reports whether err happened before any byte reached the
// server, so a retry cannot cause a duplicate send.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
