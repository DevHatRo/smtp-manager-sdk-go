package smtpmanager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testKey = "smtpk_abcdef1234.secretsecret"

// fullResponse is a complete 202 body with every field populated.
const fullResponse = `{"data":{
  "uuid":"0f4d2f6e-8d3a-4b6a-9d1e-1c2b3a4d5e6f",
  "status":"queued",
  "source":"api",
  "from":"noreply@example.com",
  "from_name":"Example",
  "to":["Alice <alice@example.org>","bob@example.org"],
  "cc":["carol@example.org"],
  "bcc":["dave@example.org"],
  "reply_to":"support@example.com",
  "subject":"Hello",
  "message_id":"<0f4d2f6e-8d3a-4b6a-9d1e-1c2b3a4d5e6f@example.com>",
  "tags":["welcome","onboarding"],
  "metadata":{"user_id":"42"},
  "attachments":[{"filename":"a.txt","content_type":"text/plain","content_id":null,"size":5},
                 {"filename":"logo.png","content_type":"image/png","content_id":"logo@example.com","size":1234}],
  "attempts":1,
  "node_id":7,
  "queue_id":"4F2A1B3C9D",
  "error":"deferred once",
  "sent_at":"2026-09-10T10:00:00Z",
  "delivered_at":"2026-09-10T10:00:05Z",
  "retry_of":"11111111-2222-3333-4444-555555555555",
  "retried_as":"66666666-7777-8888-9999-000000000000",
  "created_at":"2026-09-10T09:59:59Z",
  "updated_at":"2026-09-10T10:00:05Z"
}}`

// nullResponse is a 202 body with every nullable field null and lists empty.
const nullResponse = `{"data":{
  "uuid":"u1","status":"queued","source":"api","from":"noreply@example.com","from_name":null,
  "to":["x@example.org"],"cc":[],"bcc":[],"reply_to":null,"subject":"s","message_id":null,
  "tags":[],"metadata":{},"attachments":[],"attempts":0,"node_id":null,"queue_id":null,"error":null,
  "sent_at":null,"delivered_at":null,"retry_of":null,"retried_as":null,
  "created_at":"2026-09-10T09:59:59Z","updated_at":"2026-09-10T09:59:59Z"
}}`

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// requestLog records requests from server goroutines; every access is
// serialised so tests can inspect it while handlers may still be running.
type requestLog struct {
	mu   sync.Mutex
	reqs []capturedRequest
}

func (l *requestLog) add(r capturedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

func (l *requestLog) get(i int) capturedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[i]
}

// newTestServer returns a server that records every request and replies with
// the handler's response.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *requestLog) {
	t.Helper()
	reqs := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqs.add(capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

func respond(status int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newTestClient(t *testing.T, baseURL string, opts ...Option) *Client {
	t.Helper()
	opts = append([]Option{WithBaseURL(baseURL)}, opts...)
	c, err := New(testKey, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Never actually sleep in unit tests; record what would have been slept.
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

func basicMessage() *Message {
	return &Message{
		From:    Address{Email: "noreply@example.com", Name: "Example"},
		To:      []Address{{Email: "alice@example.org", Name: "Alice"}},
		Subject: "Hello",
		Text:    "Hi",
	}
}

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		opts   []Option
		wantOK bool
	}{
		{"empty key", "", []Option{WithBaseURL("https://x.example")}, false},
		{"blank key", "   ", []Option{WithBaseURL("https://x.example")}, false},
		{"bad prefix", "sk_live_abc", []Option{WithBaseURL("https://x.example")}, false},
		{"no base url uses the hosted default", testKey, nil, true},
		{"blank base url", testKey, []Option{WithBaseURL("  ")}, false},
		{"empty base url", testKey, []Option{WithBaseURL("")}, false},
		{"ftp base url", testKey, []Option{WithBaseURL("ftp://x.example")}, false},
		{"no host", testKey, []Option{WithBaseURL("https://")}, false},
		{"relative", testKey, []Option{WithBaseURL("x.example/api")}, false},
		{"unparseable", testKey, []Option{WithBaseURL("http://[::1")}, false},
		{"userinfo", testKey, []Option{WithBaseURL("https://user:pw@smtp.example.com")}, false},
		{"userinfo without password", testKey, []Option{WithBaseURL("https://user@smtp.example.com/api/v1/messages")}, false},
		{"suffix with CR", testKey, []Option{WithBaseURL("https://x.example"), WithUserAgentSuffix("app/1\r")}, false},
		{"suffix with LF", testKey, []Option{WithBaseURL("https://x.example"), WithUserAgentSuffix("evil\nX-Injected: 1")}, false},
		{"suffix ok", testKey, []Option{WithBaseURL("https://x.example"), WithUserAgentSuffix("app/1.0 (linux)")}, true},
		{"ok http", testKey, []Option{WithBaseURL("http://localhost:8080")}, true},
		{"ok https", testKey, []Option{WithBaseURL("https://smtp.example.com")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.key, tc.opts...)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if c == nil {
					t.Fatal("nil client")
				}
				if len(tc.opts) == 0 && c.Endpoint() != "https://api.your-email.eu/api/v1/messages" {
					t.Errorf("default endpoint = %q", c.Endpoint())
				}
				return
			}
			if tc.name == "blank base url" || tc.name == "empty base url" {
				e, _ := AsError(err)
				if e == nil || !strings.Contains(e.Message, "omit WithBaseURL") {
					t.Errorf("blank base URL should point at the default: %v", err)
				}
			}
			if err == nil {
				t.Fatal("expected error")
			}
			e, ok := AsError(err)
			if !ok || e.Code != CodeConfiguration {
				t.Fatalf("want CodeConfiguration, got %v", err)
			}
			if strings.HasPrefix(tc.name, "userinfo") && !strings.Contains(e.Message, "must not carry credentials") {
				t.Errorf("message = %q", e.Message)
			}
		})
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if DefaultBaseURL != "https://api.your-email.eu" {
		t.Errorf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	c, err := New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint() != DefaultBaseURL+"/api/v1/messages" {
		t.Errorf("Endpoint = %q", c.Endpoint())
	}
	// An explicit option still wins, and the default itself round-trips.
	if d, _ := New(testKey, WithBaseURL(DefaultBaseURL+"/")); d.Endpoint() != c.Endpoint() {
		t.Errorf("explicit default = %q", d.Endpoint())
	}
	if d, _ := New(testKey, WithBaseURL("https://smtp.example.com")); d.Endpoint() == c.Endpoint() {
		t.Error("WithBaseURL must override the default")
	}
}

func TestNew_BaseURLNormalization(t *testing.T) {
	const want = "https://smtp.example.com/api/v1/messages"
	cases := map[string]string{
		// bare origin
		"https://smtp.example.com":      want,
		"https://smtp.example.com/":     want,
		"https://smtp.example.com///":   want,
		"  https://smtp.example.com/  ": want,
		// the Endpoint shown by the console at key creation
		"https://smtp.example.com/api/v1/messages":  want,
		"https://smtp.example.com/api/v1/messages/": want,
		// partial prefixes
		"https://smtp.example.com/api":     want,
		"https://smtp.example.com/api/":    want,
		"https://smtp.example.com/api/v1":  want,
		"https://smtp.example.com/api/v1/": want,
		// a deployment under a path prefix, in every form
		"https://smtp.example.com/prefix":                  "https://smtp.example.com/prefix/api/v1/messages",
		"https://smtp.example.com/prefix/":                 "https://smtp.example.com/prefix/api/v1/messages",
		"https://smtp.example.com/prefix/api":              "https://smtp.example.com/prefix/api/v1/messages",
		"https://smtp.example.com/prefix/api/v1":           "https://smtp.example.com/prefix/api/v1/messages",
		"https://smtp.example.com/prefix/api/v1/messages":  "https://smtp.example.com/prefix/api/v1/messages",
		"https://smtp.example.com/prefix/api/v1/messages/": "https://smtp.example.com/prefix/api/v1/messages",
		// look-alikes that are not the API prefix get the full path appended
		"https://smtp.example.com/apix":         "https://smtp.example.com/apix/api/v1/messages",
		"https://smtp.example.com/api/v2":       "https://smtp.example.com/api/v2/api/v1/messages",
		"https://smtp.example.com/api/messages": "https://smtp.example.com/api/messages/api/v1/messages",
		// query, fragment and a non-default port
		"http://localhost:8080?x=1#frag":    "http://localhost:8080/api/v1/messages",
		"https://smtp.example.com:8443/api": "https://smtp.example.com:8443/api/v1/messages",
	}
	for in, want := range cases {
		c, err := New(testKey, WithBaseURL(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := c.Endpoint(); got != want {
			t.Errorf("%q: endpoint = %q, want %q", in, got, want)
		}
	}
}

func TestOptions(t *testing.T) {
	hc := &http.Client{}
	c, err := New(testKey,
		WithBaseURL("https://x.example"),
		WithHTTPClient(hc),
		WithHTTPClient(nil),
		WithTimeout(5*time.Second),
		WithTimeout(0),
		WithTimeout(-1),
		WithMaxRetries(-3),
		WithMaxRetryAfter(90*time.Second),
		WithMaxRetryAfter(0),
		WithMaxRetryAfter(-time.Second),
		WithRetryOnServerError(true),
		WithUserAgentSuffix("my-app/1.0"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.httpClient == hc || c.httpClient.Transport != hc.Transport || c.httpClient.CheckRedirect == nil {
		t.Error("WithHTTPClient should keep a redirect-refusing copy of the client; nil must not replace it")
	}
	if hc.CheckRedirect != nil {
		t.Error("caller's client must not be mutated")
	}
	if c.timeout != 5*time.Second {
		t.Errorf("timeout = %v", c.timeout)
	}
	if c.maxRetries != 0 {
		t.Errorf("maxRetries = %d, want 0 for negative input", c.maxRetries)
	}
	if !c.retryOnServerError {
		t.Error("retryOnServerError not set")
	}
	if c.maxRetryAfter != 90*time.Second {
		t.Errorf("maxRetryAfter = %v", c.maxRetryAfter)
	}
	if got := c.UserAgent(); got != "smtp-manager-sdk-go/"+Version+" my-app/1.0" {
		t.Errorf("UserAgent = %q", got)
	}

	d, _ := New(testKey, WithBaseURL("https://x.example"))
	if d.timeout != defaultTimeout || d.maxRetries != defaultMaxRetries || d.retryOnServerError || d.maxRetryAfter != 60*time.Second {
		t.Errorf("defaults: timeout=%v retries=%d 5xx=%v maxRetryAfter=%v", d.timeout, d.maxRetries, d.retryOnServerError, d.maxRetryAfter)
	}
	if d.UserAgent() != "smtp-manager-sdk-go/"+Version {
		t.Errorf("UserAgent = %q", d.UserAgent())
	}
}

func TestClient_StringRedactsKey(t *testing.T) {
	c := newTestClient(t, "https://smtp.example.com")
	const want = "smtpmanager.Client{endpoint: https://smtp.example.com/api/v1/messages, key: smtpk_abcd…}"
	// %v and %#v go through Stringer / GoStringer; both must redact.
	formatted := fmt.Sprintf("%v\n%#v", c, c)
	for _, got := range []string{c.String(), c.GoString(), strings.Split(formatted, "\n")[0], strings.Split(formatted, "\n")[1]} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, testKey) {
			t.Errorf("secret leaked: %q", got)
		}
	}
	cases := map[string]string{
		"smtpk_abcdef1234.secret": "smtpk_abcd…",
		"smtpk_ab.secret":         "smtpk_ab…",
		"smtpk_a.s":               "smtpk_a…",
		"smtpk_.s":                "smtpk_…",
		"smtpk_abcd":              "smtpk_abcd…",
		"smtpk_":                  "smtpk_…",
		"":                        "…",
	}
	for in, want := range cases {
		if got := redactKey(in); got != want {
			t.Errorf("redactKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSend_RequestShape(t *testing.T) {
	srv, reqs := newTestServer(t, respond(202, map[string]string{"X-Request-Id": "req-1"}, fullResponse))
	c := newTestClient(t, srv.URL+"/", WithUserAgentSuffix("app/2.0"))

	msg := &Message{
		From:    Address{Email: "noreply@example.com", Name: "Example"},
		To:      []Address{{Email: "alice@example.org", Name: "Alice"}, {Email: "bob@example.org"}},
		Cc:      []Address{{Email: "carol@example.org"}},
		Bcc:     []Address{{Email: "dave@example.org", Name: "Dave D"}},
		ReplyTo: Address{Email: "support@example.com", Name: "Support"},
		Subject: "Hello",
		Text:    "plain",
		HTML:    `<p>rich <img src="cid:logo@example.com"></p>`,
		Headers: map[string]string{"X-Campaign": "spring"},
		Attachments: []Attachment{
			{Filename: "a.txt", ContentType: "text/plain", Content: []byte("hello")},
			{Filename: "logo.png", ContentType: "image/png", Content: []byte{0x89, 'P', 'N', 'G'}, ContentID: "logo@example.com"},
		},
		Tags:     []string{"welcome"},
		Metadata: map[string]string{"user_id": "42"},
	}

	sent, err := c.SendWithRequestID(context.Background(), msg, "my-trace-id")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent.RequestID != "req-1" {
		t.Errorf("RequestID = %q", sent.RequestID)
	}

	if reqs.count() != 1 {
		t.Fatalf("got %d requests", reqs.count())
	}
	r := reqs.get(0)
	if r.Method != http.MethodPost {
		t.Errorf("method = %s", r.Method)
	}
	if r.Path != "/api/v1/messages" {
		t.Errorf("path = %s", r.Path)
	}
	wantHeaders := map[string]string{
		"Authorization": "Bearer " + testKey,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    "smtp-manager-sdk-go/" + Version + " app/2.0",
		"X-Request-Id":  "my-trace-id",
	}
	for k, v := range wantHeaders {
		if got := r.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}

	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, r.Body)
	}
	want := map[string]any{
		"from":     "Example <noreply@example.com>",
		"to":       []any{"Alice <alice@example.org>", "bob@example.org"},
		"cc":       []any{"carol@example.org"},
		"bcc":      []any{"Dave D <dave@example.org>"},
		"reply_to": "Support <support@example.com>",
		"subject":  "Hello",
		"text":     "plain",
		"html":     `<p>rich <img src="cid:logo@example.com"></p>`,
		"headers":  map[string]any{"X-Campaign": "spring"},
		"attachments": []any{
			map[string]any{"filename": "a.txt", "content_type": "text/plain", "content": base64.StdEncoding.EncodeToString([]byte("hello"))},
			map[string]any{"filename": "logo.png", "content_type": "image/png", "content": base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}), "content_id": "logo@example.com"},
		},
		"tags":     []any{"welcome"},
		"metadata": map[string]any{"user_id": "42"},
	}
	gotJSON, _ := json.Marshal(body)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("body mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestSend_OmitsEmptyFields(t *testing.T) {
	srv, reqs := newTestServer(t, respond(202, nil, nullResponse))
	c := newTestClient(t, srv.URL)

	sent, err := c.Send(context.Background(), &Message{
		From:    Address{Email: "noreply@example.com"},
		To:      []Address{{Email: "x@example.org"}},
		Subject: "s",
		HTML:    "<b>x</b>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.RequestID != "" {
		t.Errorf("RequestID = %q, want empty when header absent", sent.RequestID)
	}
	r := reqs.get(0)
	if r.Header.Get("X-Request-Id") != "" {
		t.Error("X-Request-Id should not be sent when empty")
	}
	if string(r.Body) != `{"from":"noreply@example.com","to":["x@example.org"],"subject":"s","html":"<b>x</b>"}` {
		t.Errorf("body = %s", r.Body)
	}
}

func TestSend_ResponseMapping(t *testing.T) {
	srv, _ := newTestServer(t, respond(202, map[string]string{"X-Request-Id": "rid"}, fullResponse))
	c := newTestClient(t, srv.URL)
	s, err := c.Send(context.Background(), basicMessage())
	if err != nil {
		t.Fatal(err)
	}
	ts := func(v string) time.Time { t, _ := time.Parse(time.RFC3339, v); return t }
	checks := []struct {
		name      string
		got, want any
	}{
		{"UUID", s.UUID, "0f4d2f6e-8d3a-4b6a-9d1e-1c2b3a4d5e6f"},
		{"Status", s.Status, "queued"},
		{"Source", s.Source, "api"},
		{"From", s.From, "noreply@example.com"},
		{"FromName", s.FromName, "Example"},
		{"To", strings.Join(s.To, "|"), "Alice <alice@example.org>|bob@example.org"},
		{"Cc", strings.Join(s.Cc, "|"), "carol@example.org"},
		{"Bcc", strings.Join(s.Bcc, "|"), "dave@example.org"},
		{"ReplyTo", s.ReplyTo, "support@example.com"},
		{"Subject", s.Subject, "Hello"},
		{"MessageID", s.MessageID, "<0f4d2f6e-8d3a-4b6a-9d1e-1c2b3a4d5e6f@example.com>"},
		{"Tags", strings.Join(s.Tags, "|"), "welcome|onboarding"},
		{"Metadata", s.Metadata["user_id"], "42"},
		{"Attachments len", len(s.Attachments), 2},
		{"Attachments[0]", s.Attachments[0], SentAttachment{Filename: "a.txt", ContentType: "text/plain", Size: 5}},
		{"Attachments[1]", s.Attachments[1], SentAttachment{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.com", Size: 1234}},
		{"Attempts", s.Attempts, 1},
		{"NodeID", *s.NodeID, 7},
		{"QueueID", s.QueueID, "4F2A1B3C9D"},
		{"Error", s.Error, "deferred once"},
		{"SentAt", *s.SentAt, ts("2026-09-10T10:00:00Z")},
		{"DeliveredAt", *s.DeliveredAt, ts("2026-09-10T10:00:05Z")},
		{"RetryOf", s.RetryOf, "11111111-2222-3333-4444-555555555555"},
		{"RetriedAs", s.RetriedAs, "66666666-7777-8888-9999-000000000000"},
		{"CreatedAt", s.CreatedAt, ts("2026-09-10T09:59:59Z")},
		{"UpdatedAt", s.UpdatedAt, ts("2026-09-10T10:00:05Z")},
		{"RequestID", s.RequestID, "rid"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestSend_ResponseNulls(t *testing.T) {
	srv, _ := newTestServer(t, respond(202, nil, nullResponse))
	c := newTestClient(t, srv.URL)
	s, err := c.Send(context.Background(), basicMessage())
	if err != nil {
		t.Fatal(err)
	}
	if s.FromName != "" || s.ReplyTo != "" || s.MessageID != "" || s.QueueID != "" || s.Error != "" || s.RetryOf != "" || s.RetriedAs != "" {
		t.Errorf("null strings should be empty: %+v", s)
	}
	if s.NodeID != nil || s.SentAt != nil || s.DeliveredAt != nil {
		t.Errorf("null pointers should be nil: %+v", s)
	}
	if len(s.Cc) != 0 || len(s.Bcc) != 0 || len(s.Tags) != 0 || len(s.Attachments) != 0 || len(s.Metadata) != 0 {
		t.Errorf("empty lists: %+v", s)
	}
	if s.Attempts != 0 || len(s.To) != 1 {
		t.Errorf("attempts/to: %+v", s)
	}
}

func TestSend_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		headers    map[string]string
		body       string
		wantCode   string
		wantMsg    string
		wantReqID  string
		wantRetry  time.Duration
		wantErrors map[string][]string
	}{
		{
			name: "401", status: 401,
			body:     `{"message":"Unauthenticated.","request_id":"r401"}`,
			wantCode: CodeAuthentication, wantMsg: "Unauthenticated.", wantReqID: "r401",
		},
		{
			name: "403", status: 403,
			body:     `{"message":"This key cannot send.","request_id":"r403"}`,
			wantCode: CodePermission, wantMsg: "This key cannot send.", wantReqID: "r403",
		},
		{
			name: "422", status: 422,
			body:     `{"message":"The given data was invalid.","errors":{"from":["The from alias is not active."],"to":["Too many recipients.","Duplicate recipient."]},"request_id":"r422"}`,
			wantCode: CodeValidation, wantMsg: "The given data was invalid.", wantReqID: "r422",
			wantErrors: map[string][]string{"from": {"The from alias is not active."}, "to": {"Too many recipients.", "Duplicate recipient."}},
		},
		{
			name: "429 numeric", status: 429, headers: map[string]string{"Retry-After": "7"},
			body:     `{"message":"Send quota exceeded.","request_id":"r429"}`,
			wantCode: CodeRateLimited, wantMsg: "Send quota exceeded.", wantReqID: "r429", wantRetry: 7 * time.Second,
		},
		{
			name: "429 no header", status: 429,
			body:     `{"message":"Too many requests.","request_id":"r429b"}`,
			wantCode: CodeRateLimited, wantMsg: "Too many requests.", wantReqID: "r429b", wantRetry: 0,
		},
		{
			name: "422 with errors of the wrong type keeps message and request id", status: 422,
			body:     `{"message":"The given data was invalid.","request_id":"r422b","errors":"not-a-map"}`,
			wantCode: CodeValidation, wantMsg: "The given data was invalid.", wantReqID: "r422b",
		},
		{
			name: "500 with message of the wrong type falls back to status text", status: 500,
			body:     `{"message":123,"request_id":"r500b"}`,
			wantCode: CodeServer, wantMsg: "Internal Server Error", wantReqID: "r500b",
		},
		{
			name: "413 non-JSON from proxy", status: 413, headers: map[string]string{"X-Request-Id": "hdr413"},
			body:     "<html><head><title>413 Request Entity Too Large</title></head></html>",
			wantCode: CodePayloadTooLarge, wantMsg: "Request Entity Too Large", wantReqID: "hdr413",
		},
		{
			name: "500", status: 500,
			body:     `{"message":"Server Error","request_id":"r500"}`,
			wantCode: CodeServer, wantMsg: "Server Error", wantReqID: "r500",
		},
		{
			name: "502 HTML", status: 502,
			body:     "<html><body><h1>502 Bad Gateway</h1></body></html>",
			wantCode: CodeServer, wantMsg: "Bad Gateway",
		},
		{
			name: "503 empty body", status: 503, body: "",
			wantCode: CodeServer, wantMsg: "Service Unavailable",
		},
		{
			name: "404 unexpected", status: 404,
			body:     `{"message":"Not Found","request_id":"r404"}`,
			wantCode: CodeUnexpectedResponse, wantMsg: "Not Found", wantReqID: "r404",
		},
		{
			name: "599 unknown status", status: 599, body: "?",
			wantCode: CodeServer, wantMsg: "unexpected HTTP status",
		},
		{
			name: "header request id when body lacks one", status: 401, headers: map[string]string{"X-Request-Id": "hdr"},
			body:     `{"message":"nope"}`,
			wantCode: CodeAuthentication, wantMsg: "nope", wantReqID: "hdr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, reqs := newTestServer(t, respond(tc.status, tc.headers, tc.body))
			c := newTestClient(t, srv.URL, WithMaxRetries(0))
			_, err := c.Send(context.Background(), basicMessage())
			if err == nil {
				t.Fatal("expected error")
			}
			e, ok := AsError(err)
			if !ok {
				t.Fatalf("not an *Error: %T %v", err, err)
			}
			if e.Code != tc.wantCode {
				t.Errorf("Code = %s, want %s", e.Code, tc.wantCode)
			}
			if e.StatusCode != tc.status {
				t.Errorf("StatusCode = %d", e.StatusCode)
			}
			if e.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", e.Message, tc.wantMsg)
			}
			if e.RequestID != tc.wantReqID {
				t.Errorf("RequestID = %q, want %q", e.RequestID, tc.wantReqID)
			}
			if e.RetryAfter != tc.wantRetry {
				t.Errorf("RetryAfter = %v, want %v", e.RetryAfter, tc.wantRetry)
			}
			if string(e.Body) != tc.body {
				t.Errorf("Body = %q", e.Body)
			}
			if len(tc.wantErrors) != len(e.Errors) {
				t.Errorf("Errors = %v, want %v", e.Errors, tc.wantErrors)
			}
			for f, msgs := range tc.wantErrors {
				if strings.Join(e.Errors[f], "|") != strings.Join(msgs, "|") {
					t.Errorf("Errors[%s] = %v, want %v", f, e.Errors[f], msgs)
				}
			}
			if reqs.count() != 1 {
				t.Errorf("expected exactly one request, got %d", reqs.count())
			}
		})
	}
}

func TestSend_RetryAfterHTTPDate(t *testing.T) {
	date := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	srv, _ := newTestServer(t, respond(429, map[string]string{"Retry-After": date}, `{"message":"slow down","request_id":"x"}`))
	c := newTestClient(t, srv.URL, WithMaxRetries(0))
	_, err := c.Send(context.Background(), basicMessage())
	e, _ := AsError(err)
	if e == nil || e.Code != CodeRateLimited {
		t.Fatalf("got %v", err)
	}
	if e.RetryAfter < 8*time.Second || e.RetryAfter > 10*time.Second {
		t.Errorf("RetryAfter = %v, want ~10s", e.RetryAfter)
	}
	if !IsRateLimited(err) {
		t.Error("IsRateLimited should be true")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		// delta-seconds: digits only, per RFC 9110
		"5":                       5 * time.Second,
		" 2 ":                     2 * time.Second,
		"0":                       0,
		"61":                      61 * time.Second,
		"31":                      31 * time.Second,
		"86399":                   86399 * time.Second,
		"86400":                   maxParsedRetryAfter,
		"86401":                   maxParsedRetryAfter,
		"100000000000":            maxParsedRetryAfter,
		"99999999999999999999999": maxParsedRetryAfter, // overflows uint64
		// not delta-seconds and not a date: treated as absent
		"":        0,
		"-3":      0,
		"+5":      0,
		"1.5":     0,
		"29.5":    0,
		"1e30":    0,
		"-1e30":   0,
		"Inf":     0,
		"+Inf":    0,
		"-Inf":    0,
		"NaN":     0,
		"0x10":    0,
		"0x1p4":   0,
		"1_000":   0,
		"garbage": 0,
		// HTTP-date
		"Thu, 10 Sep 2026 12:00:30 GMT":    30 * time.Second,
		"Thu, 10 Sep 2026 12:00:29 GMT":    29 * time.Second,
		"Thu, 10 Sep 2026 12:05:00 GMT":    5 * time.Minute,
		"Sat, 12 Sep 2026 12:00:00 GMT":    maxParsedRetryAfter,
		"Thu, 10 Sep 2026 11:59:00 GMT":    0,
		"Thursday, 10-Sep-26 12:00:10 GMT": 10 * time.Second,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSend_RetryOn429ThenSuccess(t *testing.T) {
	var n int32
	srv, reqs := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			respond(429, map[string]string{"Retry-After": "3"}, `{"message":"quota","request_id":"a"}`)(w, r)
			return
		}
		respond(202, nil, nullResponse)(w, r)
	})
	c := newTestClient(t, srv.URL)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }

	sent, err := c.Send(context.Background(), basicMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent.UUID != "u1" {
		t.Errorf("UUID = %q", sent.UUID)
	}
	if reqs.count() != 2 {
		t.Fatalf("requests = %d, want 2", reqs.count())
	}
	if string(reqs.get(0).Body) != string(reqs.get(1).Body) {
		t.Error("retry body differs from original")
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Errorf("slept = %v, want [3s]", slept)
	}
}

func TestSend_RetryAfterVersusMaxRetryAfter(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		opts       []Option
		wantSleeps []time.Duration
		wantReqs   int
		wantRA     time.Duration
	}{
		{"absent header sleeps the default", "", nil, []time.Duration{defaultRetryAfter}, 2, 0},
		{"9s honoured exactly", "9", nil, []time.Duration{9 * time.Second}, 2, 9 * time.Second},
		{"55s honoured exactly", "55", nil, []time.Duration{55 * time.Second}, 2, 55 * time.Second},
		{"60s at the cap honoured", "60", nil, []time.Duration{60 * time.Second}, 2, 60 * time.Second},
		{"61s over the cap returns immediately", "61", nil, nil, 1, 61 * time.Second},
		{"120s over the cap returns immediately", "120", nil, nil, 1, 120 * time.Second},
		{"61s honoured with a raised cap", "61", []Option{WithMaxRetryAfter(90 * time.Second)}, []time.Duration{61 * time.Second}, 2, 61 * time.Second},
		{"9s refused with a lowered cap", "9", []Option{WithMaxRetryAfter(5 * time.Second)}, nil, 1, 9 * time.Second},
		{"huge value returns immediately with a bounded RetryAfter", "100000000000", nil, nil, 1, maxParsedRetryAfter},
		{"unparseable header is treated as absent", "1e30", nil, []time.Duration{defaultRetryAfter}, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.header != "" {
				headers["Retry-After"] = tc.header
			}
			srv, reqs := newTestServer(t, respond(429, headers, `{"message":"quota","request_id":"a"}`))
			c := newTestClient(t, srv.URL, append([]Option{WithMaxRetries(1)}, tc.opts...)...)
			var slept []time.Duration
			c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			_, err := c.Send(context.Background(), basicMessage())
			e, _ := AsError(err)
			if e == nil || e.Code != CodeRateLimited {
				t.Fatalf("got %v", err)
			}
			if e.RetryAfter != tc.wantRA {
				t.Errorf("RetryAfter = %v, want %v", e.RetryAfter, tc.wantRA)
			}
			if fmt.Sprint(slept) != fmt.Sprint(tc.wantSleeps) {
				t.Errorf("slept %v, want %v", slept, tc.wantSleeps)
			}
			if reqs.count() != tc.wantReqs {
				t.Errorf("requests = %d, want %d", reqs.count(), tc.wantReqs)
			}
		})
	}
}

func TestSend_RetriesExhausted(t *testing.T) {
	srv, reqs := newTestServer(t, respond(429, map[string]string{"Retry-After": "1"}, `{"message":"quota","request_id":"a"}`))
	c := newTestClient(t, srv.URL, WithMaxRetries(2))
	_, err := c.Send(context.Background(), basicMessage())
	if !IsRateLimited(err) {
		t.Fatalf("got %v", err)
	}
	if reqs.count() != 3 {
		t.Errorf("requests = %d, want 3 (1 + 2 retries)", reqs.count())
	}
}

func TestSend_RetrySleepHonoursContext(t *testing.T) {
	srv, reqs := newTestServer(t, respond(429, map[string]string{"Retry-After": "5"}, `{"message":"quota","request_id":"a"}`))
	c := newTestClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }
	_, err := c.Send(ctx, basicMessage())
	e, _ := AsError(err)
	if e == nil || e.Code != CodeNetwork || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	// What the last attempt learned survives the cancellation.
	if e.RequestID != "a" || e.RetryAfter != 5*time.Second {
		t.Errorf("RequestID = %q, RetryAfter = %v; want the 429's", e.RequestID, e.RetryAfter)
	}
	var inner *Error
	joined, ok := e.Cause.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Cause should join the context error and the last attempt's error, got %T", e.Cause)
	}
	for _, c := range joined.Unwrap() {
		if errors.As(c, &inner) && inner.Code == CodeRateLimited {
			break
		}
	}
	if inner == nil || inner.Code != CodeRateLimited || inner.StatusCode != 429 {
		t.Errorf("last attempt's *Error not reachable as cause: %v", err)
	}
	if reqs.count() != 1 {
		t.Errorf("requests = %d, want 1", reqs.count())
	}

}

func TestSend_DeadlineExpiresDuringRetrySleep(t *testing.T) {
	// The first response is a 429 asking for 5s; the context deadline fires
	// while the real sleep is waiting, which must come back as a timeout.
	var n int32
	srv, reqs := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			respond(429, map[string]string{"Retry-After": "5"}, `{"message":"quota","request_id":"a"}`)(w, r)
			return
		}
		respond(202, nil, nullResponse)(w, r)
	})
	c := newTestClient(t, srv.URL)
	c.sleep = sleepContext
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Send(ctx, basicMessage())
	e, _ := AsError(err)
	if e == nil || e.Code != CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed %v: expected to wait for the deadline, not the full Retry-After", elapsed)
	}
	if reqs.count() != 1 {
		t.Errorf("requests = %d, want 1", reqs.count())
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Errorf("zero sleep: %v", err)
	}
	start := time.Now()
	if err := sleepContext(context.Background(), 5*time.Millisecond); err != nil {
		t.Errorf("short sleep: %v", err)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Error("did not sleep")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sleep: %v", err)
	}
}

func TestSend_NoRetryOn5xxByDefault(t *testing.T) {
	srv, reqs := newTestServer(t, respond(500, nil, `{"message":"boom","request_id":"a"}`))
	c := newTestClient(t, srv.URL)
	_, err := c.Send(context.Background(), basicMessage())
	if e, _ := AsError(err); e == nil || e.Code != CodeServer {
		t.Fatalf("got %v", err)
	}
	if reqs.count() != 1 {
		t.Errorf("requests = %d, want 1", reqs.count())
	}
}

func TestSend_RetryOn5xxWhenOptedIn(t *testing.T) {
	var n int32
	srv, reqs := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			respond(503, nil, "upstream down")(w, r)
			return
		}
		respond(202, nil, nullResponse)(w, r)
	})
	c := newTestClient(t, srv.URL, WithRetryOnServerError(true), WithMaxRetries(2))
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.Send(context.Background(), basicMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if reqs.count() != 3 {
		t.Errorf("requests = %d, want 3", reqs.count())
	}
	if len(slept) != 2 || slept[0] != serverErrorBackoff || slept[1] != 2*serverErrorBackoff {
		t.Errorf("slept = %v", slept)
	}
}

func TestSend_NoRetryOn4xx(t *testing.T) {
	for _, status := range []int{401, 403, 413, 422} {
		srv, reqs := newTestServer(t, respond(status, nil, `{"message":"no","request_id":"a"}`))
		c := newTestClient(t, srv.URL, WithMaxRetries(3), WithRetryOnServerError(true))
		if _, err := c.Send(context.Background(), basicMessage()); err == nil {
			t.Fatalf("%d: expected error", status)
		}
		if reqs.count() != 1 {
			t.Errorf("%d: requests = %d, want 1", status, reqs.count())
		}
	}
}

func TestSend_NetworkErrorRetry(t *testing.T) {
	// Reserve a port, then close the listener so dialing it is refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c := newTestClient(t, "http://"+addr, WithMaxRetries(2))
	var slept int
	c.sleep = func(context.Context, time.Duration) error { slept++; return nil }
	_, err = c.Send(context.Background(), basicMessage())
	e, ok := AsError(err)
	if !ok || e.Code != CodeNetwork {
		t.Fatalf("got %v", err)
	}
	if e.StatusCode != 0 || e.Cause == nil {
		t.Errorf("StatusCode=%d Cause=%v", e.StatusCode, e.Cause)
	}
	if slept != 2 {
		t.Errorf("retried %d times, want 2", slept)
	}
	if !strings.Contains(err.Error(), "network_error") {
		t.Errorf("Error() = %q", err)
	}
}

func TestSend_NetworkErrorThenSuccess(t *testing.T) {
	srv, _ := newTestServer(t, respond(202, nil, nullResponse))
	var n int32
	// Transport that fails the first dial and passes the rest through.
	failing := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	c := newTestClient(t, srv.URL, WithHTTPClient(&http.Client{Transport: failing}))
	if _, err := c.Send(context.Background(), basicMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("dials = %d, want 2", got)
	}
}

func TestSend_NonDialNetworkErrorNotRetried(t *testing.T) {
	// The server accepts the connection then drops it: the request may have
	// reached it, so the SDK must not retry.
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, WithMaxRetries(3))
	_, err := c.Send(context.Background(), basicMessage())
	e, ok := AsError(err)
	if !ok || e.Code != CodeNetwork {
		t.Fatalf("got %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestSend_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(202)
	})
	defer close(release)

	t.Run("client timeout", func(t *testing.T) {
		c := newTestClient(t, srv.URL, WithTimeout(20*time.Millisecond), WithMaxRetries(3))
		_, err := c.Send(context.Background(), basicMessage())
		e, ok := AsError(err)
		if !ok || e.Code != CodeTimeout {
			t.Fatalf("got %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("should unwrap to DeadlineExceeded: %v", err)
		}
	})
	t.Run("context deadline wins", func(t *testing.T) {
		c := newTestClient(t, srv.URL, WithTimeout(time.Minute))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := c.Send(ctx, basicMessage())
		if e, _ := AsError(err); e == nil || e.Code != CodeTimeout {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		c := newTestClient(t, srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(10 * time.Millisecond); cancel() }()
		_, err := c.Send(ctx, basicMessage())
		e, _ := AsError(err)
		if e == nil || e.Code != CodeNetwork || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		var urlErr *url.Error
		if !errors.As(err, &urlErr) {
			t.Errorf("transport *url.Error should stay reachable as the cause: %v", err)
		}
	})
	t.Run("client timeout keeps transport cause", func(t *testing.T) {
		c := newTestClient(t, srv.URL, WithTimeout(20*time.Millisecond))
		_, err := c.Send(context.Background(), basicMessage())
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v", err)
		}
	})
}

func TestContextError(t *testing.T) {
	transport := &url.Error{Op: "Post", URL: "http://x", Err: errors.New("boom")}
	e := contextError(context.Canceled, transport)
	if e.Code != CodeNetwork || !errors.Is(e, context.Canceled) || !errors.Is(e, transport) {
		t.Errorf("unrelated transport error should be joined: %v", e)
	}
	e = contextError(context.DeadlineExceeded, nil)
	if e.Code != CodeTimeout || !errors.Is(e, context.DeadlineExceeded) {
		t.Errorf("nil transport: %v", e)
	}
}

func TestSend_UnexpectedSuccessBodies(t *testing.T) {
	cases := map[string]string{
		"not json":    "<html>ok</html>",
		"no data":     `{"message":"accepted"}`,
		"data null":   `{"data":null}`,
		"data scalar": `{"data":"x"}`,
		"empty":       "",
		"array":       `[]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := newTestServer(t, respond(202, map[string]string{"X-Request-Id": "r"}, body))
			c := newTestClient(t, srv.URL)
			_, err := c.Send(context.Background(), basicMessage())
			e, ok := AsError(err)
			if !ok || e.Code != CodeUnexpectedResponse {
				t.Fatalf("got %v", err)
			}
			if e.StatusCode != 202 || e.RequestID != "r" || string(e.Body) != body {
				t.Errorf("%+v", e)
			}
		})
	}
}

func TestSend_InvalidInput(t *testing.T) {
	srv, reqs := newTestServer(t, respond(202, nil, nullResponse))
	c := newTestClient(t, srv.URL)
	bad := []*Message{
		nil,
		{From: Address{Email: "a@b", Name: "Evil <x@y>"}, To: []Address{{Email: "c@d"}}},
		{From: Address{Email: "a@b"}, To: []Address{{Email: "c@d", Name: "Line\nBreak"}}},
		{From: Address{Email: "a@b"}, Cc: []Address{{Email: "c@d", Name: "CR\r"}}},
		{From: Address{Email: "a@b"}, Bcc: []Address{{Email: "c@d", Name: ">"}}},
		{From: Address{Email: "a@b"}, ReplyTo: Address{Email: "c@d", Name: "<"}},
		{From: Address{Email: "a@b>"}, To: []Address{{Email: "c@d"}}},
		{From: Address{Email: "a@b"}, To: []Address{{Email: "c@d\r\n"}}},
		{From: Address{Email: "a@b"}, ReplyTo: Address{Email: "<c@d>"}},
	}
	for i, m := range bad {
		_, err := c.Send(context.Background(), m)
		e, ok := AsError(err)
		if !ok || e.Code != CodeInvalidInput {
			t.Errorf("case %d: got %v", i, err)
		}
	}
	if reqs.count() != 0 {
		t.Errorf("no request should be sent, got %d", reqs.count())
	}
}

func TestSend_BodiesAreBounded(t *testing.T) {
	t.Run("error body", func(t *testing.T) {
		big := strings.Repeat("x", maxErrorBody+100)
		srv, _ := newTestServer(t, respond(502, nil, big))
		c := newTestClient(t, srv.URL)
		_, err := c.Send(context.Background(), basicMessage())
		e, _ := AsError(err)
		if e == nil || e.Code != CodeServer || len(e.Body) != maxErrorBody {
			t.Fatalf("got %v (Body len %d)", err, len(e.Body))
		}
	})
	t.Run("oversized success body", func(t *testing.T) {
		// Over the 1 MiB read limit: truncated JSON cannot parse, and the
		// stored Body is capped at the error limit.
		big := `{"data":{"uuid":"u","subject":"` + strings.Repeat("y", maxSuccessBody+10) + `"}}`
		srv, _ := newTestServer(t, respond(202, nil, big))
		c := newTestClient(t, srv.URL)
		_, err := c.Send(context.Background(), basicMessage())
		e, _ := AsError(err)
		if e == nil || e.Code != CodeUnexpectedResponse || len(e.Body) != maxErrorBody {
			t.Fatalf("got %v (Body len %d)", err, len(e.Body))
		}
	})
	t.Run("large valid success body", func(t *testing.T) {
		body := `{"data":{"uuid":"u","subject":"` + strings.Repeat("z", 200*1024) + `"}}`
		srv, _ := newTestServer(t, respond(202, nil, body))
		c := newTestClient(t, srv.URL)
		sent, err := c.Send(context.Background(), basicMessage())
		if err != nil || len(sent.Subject) != 200*1024 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSend_RedirectIsNotFollowed(t *testing.T) {
	var target int32
	srv, reqs := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			atomic.AddInt32(&target, 1)
			respond(202, nil, nullResponse)(w, r)
			return
		}
		w.Header().Set("Location", "/elsewhere")
		w.Header().Set("X-Request-Id", "r301")
		w.WriteHeader(http.StatusMovedPermanently)
		_, _ = io.WriteString(w, "<html>moved</html>")
	})

	check := func(t *testing.T, c *Client) {
		t.Helper()
		_, err := c.Send(context.Background(), basicMessage())
		e, ok := AsError(err)
		if !ok || e.Code != CodeUnexpectedResponse || e.StatusCode != 301 {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(e.Message, "/elsewhere") || !strings.Contains(e.Message, "check the base URL") {
			t.Errorf("Message = %q", e.Message)
		}
		if e.RequestID != "r301" {
			t.Errorf("RequestID = %q", e.RequestID)
		}
		if atomic.LoadInt32(&target) != 0 {
			t.Error("redirect target must not be requested")
		}
	}
	t.Run("default client", func(t *testing.T) { check(t, newTestClient(t, srv.URL, WithMaxRetries(3))) })
	t.Run("caller-supplied client", func(t *testing.T) {
		check(t, newTestClient(t, srv.URL, WithHTTPClient(&http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()})))
	})
	t.Run("caller's own CheckRedirect is kept", func(t *testing.T) {
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("custom") }}
		c := newTestClient(t, srv.URL, WithHTTPClient(hc))
		_, err := c.Send(context.Background(), basicMessage())
		e, _ := AsError(err)
		if e == nil || e.Code != CodeNetwork || !strings.Contains(err.Error(), "custom") {
			t.Fatalf("got %v", err)
		}
	})
	if reqs.count() != 3 {
		t.Errorf("requests = %d, want 3 (one per subtest)", reqs.count())
	}
}

func TestSendWithRequestID_Validation(t *testing.T) {
	srv, reqs := newTestServer(t, respond(202, nil, nullResponse))
	c := newTestClient(t, srv.URL)
	for _, id := range []string{"a b", "x\r\nX-Injected: 1", "id/with/slash", "ünïcode", strings.Repeat("a", 65), "id:colon"} {
		_, err := c.SendWithRequestID(context.Background(), basicMessage(), id)
		e, ok := AsError(err)
		if !ok || e.Code != CodeInvalidInput || !strings.Contains(e.Message, "request id") {
			t.Errorf("%q: got %v", id, err)
		}
	}
	if reqs.count() != 0 {
		t.Fatalf("no request should be sent for an invalid id, got %d", reqs.count())
	}
	for _, id := range []string{"a", "trace-7f3a9c", "ok-id.1_2", strings.Repeat("Z", 64)} {
		if _, err := c.SendWithRequestID(context.Background(), basicMessage(), id); err != nil {
			t.Errorf("%q: unexpected %v", id, err)
		}
	}
	if reqs.count() != 4 {
		t.Errorf("requests = %d, want 4", reqs.count())
	}
}

func TestSend_Concurrent(t *testing.T) {
	srv, reqs := newTestServer(t, respond(202, nil, nullResponse))
	c := newTestClient(t, srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Send(context.Background(), basicMessage()); err != nil {
				t.Errorf("Send: %v", err)
			}
		}()
	}
	wg.Wait()
	if reqs.count() != 20 {
		t.Errorf("requests = %d", reqs.count())
	}
}
