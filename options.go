package smtpmanager

import (
	"net/http"
	"time"
)

// Option configures a Client. Options are applied in order by New.
type Option func(*Client)

// WithBaseURL sets where to send. It is required. Paste either the Endpoint
// the console showed when the key was created
// ("https://smtp.example.com/api/v1/messages") or the API origin
// ("https://smtp.example.com"); "/api" and "/api/v1" prefixes and trailing
// slashes are also understood. All forms resolve to the same
// "/api/v1/messages" URL, see Client.Endpoint.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		c.baseURL = baseURL
	}
}

// WithHTTPClient sets a custom *http.Client, giving full control over the
// transport, TLS configuration, proxies and connection pooling. The client's
// own Timeout, if set, still applies in addition to the SDK timeout.
//
// The SDK uses a shallow copy that never follows redirects (the send
// endpoint is not redirected; a 3xx is reported as CodeUnexpectedResponse),
// unless hc already sets its own CheckRedirect, which is kept.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc == nil {
			return
		}
		cp := *hc
		if cp.CheckRedirect == nil {
			cp.CheckRedirect = neverFollowRedirects
		}
		c.httpClient = &cp
	}
}

// WithTimeout sets the per-attempt request timeout. The default is 30
// seconds. A context with a shorter deadline passed to Send takes precedence.
// Non-positive durations are ignored.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithMaxRetries sets how many times a failed request is retried after the
// first attempt. The default is 2. Negative values are treated as 0.
//
// Only rate-limited responses (HTTP 429) and dial errors, where the request
// never reached the server, are retried by default; see
// WithRetryOnServerError for 5xx responses.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithMaxRetryAfter sets the longest Retry-After the client is willing to
// wait out before retrying a rate-limited (HTTP 429) request. The default is
// 60 seconds, matching the API's per-minute send quota. A 429 asking for
// more than this is returned immediately as a CodeRateLimited error, with
// Error.RetryAfter carrying the server's value, so the caller can decide.
// Non-positive durations are ignored.
//
// Together with WithTimeout and WithMaxRetries this bounds the wall time of
// a single Send: at most (1 + MaxRetries) attempts of Timeout each, plus at
// most MaxRetries waits of MaxRetryAfter each.
func WithMaxRetryAfter(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.maxRetryAfter = d
		}
	}
}

// WithRetryOnServerError enables retrying on HTTP 5xx responses.
//
// It is off by default because the API has no idempotency key yet: a 5xx
// returned after the server accepted the message could mean the message was
// queued, and a retry would send it twice. Enable it only when duplicate
// delivery is acceptable.
func WithRetryOnServerError(enabled bool) Option {
	return func(c *Client) {
		c.retryOnServerError = enabled
	}
}

// WithUserAgentSuffix appends a suffix to the User-Agent header, separated by
// a space, so requests can be attributed to the calling application, for
// example "my-app/1.4.2". A suffix containing CR or LF makes New fail with
// CodeConfiguration.
func WithUserAgentSuffix(suffix string) Option {
	return func(c *Client) {
		c.userAgentSuffix = suffix
	}
}
