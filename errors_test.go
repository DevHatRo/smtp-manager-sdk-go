package smtpmanager

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestError_Error(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			"code only",
			&Error{Code: CodeNetwork},
			"smtpmanager: network_error",
		},
		{
			"status and message",
			&Error{Code: CodeAuthentication, StatusCode: 401, Message: "Unauthenticated."},
			"smtpmanager: authentication_error (HTTP 401): Unauthenticated.",
		},
		{
			"request id",
			&Error{Code: CodeServer, StatusCode: 500, Message: "boom", RequestID: "abc"},
			"smtpmanager: server_error (HTTP 500): boom (request_id=abc)",
		},
		{
			"validation fields sorted",
			&Error{Code: CodeValidation, StatusCode: 422, Message: "invalid", RequestID: "r",
				Errors: map[string][]string{"to": {"too many", "duplicate"}, "from": {"not active"}}},
			"smtpmanager: validation_error (HTTP 422): invalid [from: not active; to: too many, duplicate] (request_id=r)",
		},
		{
			"cause",
			&Error{Code: CodeTimeout, Message: "request timed out", Cause: errors.New("dial tcp: i/o timeout")},
			"smtpmanager: timeout: request timed out: dial tcp: i/o timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestError_Unwrap(t *testing.T) {
	cause := errors.New("root")
	e := &Error{Code: CodeNetwork, Cause: cause}
	if !errors.Is(e, cause) {
		t.Error("errors.Is should find the cause")
	}
	if (&Error{Code: CodeNetwork}).Unwrap() != nil {
		t.Error("Unwrap without cause should be nil")
	}
}

func TestErrorHelpers(t *testing.T) {
	wrap := func(e *Error) error { return fmt.Errorf("outer: %w", e) }
	v := wrap(&Error{Code: CodeValidation})
	r := wrap(&Error{Code: CodeRateLimited, RetryAfter: time.Second})
	a := wrap(&Error{Code: CodeAuthentication})
	plain := errors.New("plain")

	if !IsValidation(v) || IsValidation(r) || IsValidation(a) || IsValidation(plain) || IsValidation(nil) {
		t.Error("IsValidation")
	}
	if !IsRateLimited(r) || IsRateLimited(v) || IsRateLimited(plain) {
		t.Error("IsRateLimited")
	}
	if !IsAuthentication(a) || IsAuthentication(v) || IsAuthentication(plain) {
		t.Error("IsAuthentication")
	}

	e, ok := AsError(r)
	if !ok || e.RetryAfter != time.Second {
		t.Errorf("AsError = %v, %v", e, ok)
	}
	if e, ok := AsError(plain); ok || e != nil {
		t.Error("AsError on a plain error should be nil,false")
	}
	if e, ok := AsError(nil); ok || e != nil {
		t.Error("AsError(nil) should be nil,false")
	}
}
