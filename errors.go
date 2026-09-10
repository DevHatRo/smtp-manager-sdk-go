package smtpmanager

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Error codes for machine-readable classification of *Error values.
const (
	// CodeAuthentication is returned for HTTP 401: the API key is missing,
	// malformed or revoked.
	CodeAuthentication = "authentication_error"
	// CodePermission is returned for HTTP 403: the API key lacks the "send"
	// ability.
	CodePermission = "permission_error"
	// CodeValidation is returned for HTTP 422; Error.Errors carries the
	// per-field messages.
	CodeValidation = "validation_error"
	// CodeRateLimited is returned for HTTP 429 once retries are exhausted;
	// Error.RetryAfter carries the server's Retry-After hint.
	CodeRateLimited = "rate_limited"
	// CodePayloadTooLarge is returned for HTTP 413: the request body exceeded
	// the proxy limit (12 MB). The body is usually not JSON.
	CodePayloadTooLarge = "payload_too_large"
	// CodeServer is returned for HTTP 5xx responses.
	CodeServer = "server_error"
	// CodeNetwork is returned when the request could not be completed at the
	// transport level (DNS, dial, connection reset, cancelled context).
	CodeNetwork = "network_error"
	// CodeTimeout is returned when the request exceeded the client timeout or
	// the context deadline.
	CodeTimeout = "timeout"
	// CodeInvalidInput is returned before any request is made when the
	// message cannot be encoded (nil message, unsafe display name).
	CodeInvalidInput = "invalid_input"
	// CodeConfiguration is returned by New for an unusable API key or base URL.
	CodeConfiguration = "configuration_error"
	// CodeUnexpectedResponse is returned for any response the SDK cannot
	// interpret (unknown status, a redirect, malformed JSON, missing "data").
	// When StatusCode is 2xx (202 in particular) the server most likely
	// queued the message and only the reply was unreadable: do not re-send.
	CodeUnexpectedResponse = "unexpected_response"
)

// Error is the error type returned by every operation in this package.
type Error struct {
	// Code is one of the Code* constants.
	Code string
	// StatusCode is the HTTP status code, or 0 when no response was received.
	StatusCode int
	// Message is a human-readable description; for API errors it is the
	// server's "message" field.
	Message string
	// RequestID is the server-side request identifier from the response body
	// or the X-Request-Id header, when available. Quote it in support requests.
	RequestID string
	// Errors holds per-field validation messages for CodeValidation.
	Errors map[string][]string
	// RetryAfter is the server's Retry-After hint for CodeRateLimited, or 0.
	RetryAfter time.Duration
	// Body is the raw response body for API errors, useful when it is not JSON.
	Body []byte
	// Cause is the underlying error, if any.
	Cause error

	// retryable marks a CodeNetwork error that happened before the request
	// reached the server (dial failure), so it is safe to retry.
	retryable bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("smtpmanager: ")
	b.WriteString(e.Code)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Errors) > 0 {
		fields := make([]string, 0, len(e.Errors))
		for f := range e.Errors {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		b.WriteString(" [")
		for i, f := range fields {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(f)
			b.WriteString(": ")
			b.WriteString(strings.Join(e.Errors[f], ", "))
		}
		b.WriteString("]")
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	}
	return b.String()
}

// Unwrap returns the underlying cause for use with errors.Is and errors.As.
func (e *Error) Unwrap() error {
	return e.Cause
}

// AsError extracts the *Error from err, if err is or wraps one.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsValidation reports whether err is a CodeValidation error (HTTP 422).
func IsValidation(err error) bool {
	return hasCode(err, CodeValidation)
}

// IsRateLimited reports whether err is a CodeRateLimited error (HTTP 429).
func IsRateLimited(err error) bool {
	return hasCode(err, CodeRateLimited)
}

// IsAuthentication reports whether err is a CodeAuthentication error (HTTP 401).
func IsAuthentication(err error) bool {
	return hasCode(err, CodeAuthentication)
}

func hasCode(err error, code string) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}
