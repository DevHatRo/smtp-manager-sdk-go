# smtp-manager-sdk-go

A zero-dependency Go SDK for the **SMTP Manager** transactional send API by
[DevHat](https://github.com/DevHatRo). SMTP Manager is a multi-tenant sending
platform: you register a domain, verify it, create aliases, and send through
either SMTP or this HTTP API. The SDK wraps the single send endpoint
(`POST /api/v1/messages`) with typed messages, typed errors and a small retry
policy for rate limits and dial failures.

**Repository:** [github.com/DevHatRo/smtp-manager-sdk-go](https://github.com/DevHatRo/smtp-manager-sdk-go)

- Standard library only (Go 1.22+)
- `context.Context` on every call, per-attempt timeout, concurrent-safe client
- Typed `*Error` with `Code`, HTTP status, `RequestID`, per-field validation messages and `Retry-After`
- Retries on `429 Too Many Requests` (honouring `Retry-After`) and on dial errors, never on ambiguous failures
- Accepts the Endpoint the console shows or the bare API origin; `String()` redacts the key so a client is safe to log
- Unit tests against `httptest.Server`; opt-in integration test against a real instance

## Installation

```bash
go get github.com/DevHatRo/smtp-manager-sdk-go
```

## Quick start

This mirrors `integration_test.go` (only the environment lookups are inlined),
so it is known to work against a live instance.

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "time"

    smtpmanager "github.com/DevHatRo/smtp-manager-sdk-go"
)

func main() {
    // Talks to the hosted platform (https://api.your-email.eu). Self-hosted?
    // Add smtpmanager.WithBaseURL(...) with the Endpoint your console showed.
    client, err := smtpmanager.New(os.Getenv("SMTP_MANAGER_API_KEY"))
    if err != nil {
        log.Fatal(err)
    }

    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    sent, err := client.Send(ctx, &smtpmanager.Message{
        From:    smtpmanager.Address{Email: os.Getenv("SMTP_MANAGER_FROM"), Name: "SDK Integration"},
        To:      []smtpmanager.Address{{Email: os.Getenv("SMTP_MANAGER_TO"), Name: "Integration Recipient"}},
        Cc:      []smtpmanager.Address{{Email: "accounts@example.net"}},
        Subject: "smtp-manager-sdk-go integration " + time.Now().UTC().Format(time.RFC3339),
        Text:    "Sent by the smtp-manager-sdk-go integration test.",
        HTML:    "<p>Sent by the <b>smtp-manager-sdk-go</b> integration test.</p>",
        Attachments: []smtpmanager.Attachment{
            {Filename: "hello.txt", ContentType: "text/plain", Content: []byte("hello from the SDK\n")},
        },
        Tags:     []string{"sdk", "integration"},
        Metadata: map[string]string{"suite": "integration", "sdk": "go"},
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(sent.UUID, sent.Status, sent.MessageID, sent.RequestID)
}
```

`Send` returns as soon as the API has **accepted** the message (HTTP 202). The
returned `SentMessage` is the message resource at that instant: `Status` is
normally `queued`, and `SentAt`, `DeliveredAt`, `NodeID` and `QueueID` are
filled in later by the delivery pipeline.

## Full composition

```go
logo, err := smtpmanager.AttachmentFromFile("assets/logo.png") // content type from the extension
if err != nil {
    log.Fatal(err)
}
logo.ContentID = "logo@example.com" // makes it an inline part addressable as cid:logo@example.com

invoice, err := smtpmanager.AttachmentFromFile("out/invoice-1042.pdf")
if err != nil {
    log.Fatal(err)
}

sent, err := client.SendWithRequestID(ctx, &smtpmanager.Message{
    From:    smtpmanager.Address{Email: "noreply@example.com", Name: "Example"},
    To:      []smtpmanager.Address{{Email: "ann@example.org", Name: "Ann Example"}},
    Cc:      []smtpmanager.Address{{Email: "accounts@example.org"}},
    Bcc:     []smtpmanager.Address{{Email: "archive@example.com"}},
    ReplyTo: smtpmanager.Address{Email: "billing@example.com", Name: "Example Billing"}, // optional
    Subject: "Invoice #1042",
    Text:    "Your invoice #1042 is attached.",
    HTML: `<p><img src="cid:logo@example.com" alt="Example"></p>
           <p>Your invoice <b>#1042</b> is attached.</p>`,
    Headers: map[string]string{
        "X-Entity-Ref-ID": "invoice-1042",
    },
    Attachments: []smtpmanager.Attachment{logo, invoice},
    Tags:        []string{"billing", "invoice"},
    Metadata: map[string]string{
        "customer_id": "c_8812",
        "invoice_id":  "1042",
    },
}, "trace-7f3a9c") // optional X-Request-Id (^[A-Za-z0-9._-]{1,64}$), echoed back as sent.RequestID
```

Notes on the wire format, all enforced by the API rather than the SDK:

- `From` must be an **active alias on a verified domain** of the API key's organization.
- Addresses are sent as `Name <email>` or a bare `email`. A name or email containing `<`, `>`, CR or LF would break that rendering and is refused locally with `CodeInvalidInput`; everything else (address syntax, alias ownership, domain verification) is validated server-side so the SDK never drifts from the server rules. `ReplyTo` is optional: leave its `Email` empty to omit it.
- `Headers` may not set addressing headers, `Received`, `Message-ID` or `DKIM-*`.
- Attachments are base64-encoded by the SDK. Executable extensions are refused. `ContentID` must look like `local@domain`.
- `Metadata` key order is not preserved server-side.

## Error handling

Every error is a `*smtpmanager.Error`. Use `errors.As` or the helpers.

```go
sent, err := client.Send(ctx, msg)
if err != nil {
    var apiErr *smtpmanager.Error
    if !errors.As(err, &apiErr) {
        log.Fatal(err) // cannot happen with this SDK, but be safe
    }

    switch apiErr.Code {
    case smtpmanager.CodeValidation: // HTTP 422
        for field, msgs := range apiErr.Errors {
            fmt.Printf("%s: %s\n", field, strings.Join(msgs, "; "))
        }
    case smtpmanager.CodeRateLimited: // HTTP 429 after retries were exhausted
        fmt.Println("retry after", apiErr.RetryAfter)
    case smtpmanager.CodeAuthentication, smtpmanager.CodePermission: // 401 / 403
        fmt.Println("check the API key and its abilities:", apiErr.Message)
    case smtpmanager.CodePayloadTooLarge: // 413 from the proxy, body is not JSON
        fmt.Println("shrink the attachments")
    case smtpmanager.CodeTimeout, smtpmanager.CodeNetwork:
        fmt.Println("transport problem:", apiErr.Cause)
    default: // CodeServer, CodeUnexpectedResponse, CodeInvalidInput, CodeConfiguration
        fmt.Println(apiErr)
    }
    fmt.Println("quote this in support requests:", apiErr.RequestID)
    return
}

// or, for the common cases:
if smtpmanager.IsValidation(err) { /* ... */ }
if smtpmanager.IsRateLimited(err) { /* ... */ }
if smtpmanager.IsAuthentication(err) { /* ... */ }
if apiErr, ok := smtpmanager.AsError(err); ok { fmt.Println(apiErr.StatusCode) }
```

`Error()` renders a single line such as
`smtpmanager: validation_error (HTTP 422): The given data was invalid. [from: The from alias is not active.] (request_id=01J9...)`,
and `Unwrap()` exposes the transport cause, so `errors.Is(err, context.DeadlineExceeded)` works.

| Code | When | Fields set |
|---|---|---|
| `CodeAuthentication` | 401: key missing, malformed or revoked | `Message`, `RequestID` |
| `CodePermission` | 403: key lacks the `send` ability | `Message`, `RequestID` |
| `CodeValidation` | 422 | `Message`, `RequestID`, `Errors` |
| `CodeRateLimited` | 429: send quota, or a burst of failed authentications | `Message`, `RequestID`, `RetryAfter` (bounded to 24 h) |
| `CodePayloadTooLarge` | 413: body over 12 MB (refused by the proxy, body not JSON) | `Body` |
| `CodeServer` | 5xx | `Message` (server's, or the status text), `Body` |
| `CodeNetwork` | DNS/dial/reset, or the context was cancelled | `Cause` |
| `CodeTimeout` | client timeout or context deadline | `Cause` |
| `CodeInvalidInput` | nil message or unsafe display name; no request made | `Message` |
| `CodeConfiguration` | `New` rejected the key or base URL | `Message` |
| `CodeUnexpectedResponse` | any response the SDK cannot interpret: a redirect, malformed JSON, missing `data` | `StatusCode`, `Body` |

> **A `CodeUnexpectedResponse` with a 2xx `StatusCode` (202 in particular) means the server most likely queued the message and only the reply was unreadable — do not re-send it.** Log the `RequestID` and check the console instead.

## Retries and rate limits

Each `Send` makes up to `1 + MaxRetries` attempts (default `2` retries), rebuilding the request each time:

- **429** — every 429 is treated the same (the send quota and the failed-authentication throttle are both per minute). If the server's `Retry-After` (delta-seconds or HTTP-date; 1 s when absent or unparseable) is within `MaxRetryAfter` (default 60 s, the API's per-minute window), the client sleeps exactly that long, honouring context cancellation, then retries. If it is longer, the `CodeRateLimited` error is returned **immediately without sleeping** — retrying before the limiter's window would only waste an attempt — with `RetryAfter` carrying the server's value (bounded to 24 h so it is always safe to sleep on) for you to decide. A context cancelled during the wait comes back as `CodeNetwork`/`CodeTimeout` that still carries the 429's `RequestID` and `RetryAfter`, with the 429 `*Error` reachable through `Unwrap`.
- **3xx** — never followed. The send endpoint is not redirected, so a redirect means the base URL is wrong; it is reported as `CodeUnexpectedResponse` with the `Location` in the message. A caller-supplied `*http.Client` is used through a shallow copy with the same rule unless it sets its own `CheckRedirect`.
- **Dial errors** (connection refused, DNS failure) — retried with a short backoff, because the request never reached the server.
- **5xx** — *not* retried unless `WithRetryOnServerError(true)`.
- **Timeouts, connection resets, 4xx** — never retried.

**Wall-time bound.** A single `Send` makes at most `1 + MaxRetries` attempts
of at most `Timeout` each, separated by at most `MaxRetries` waits of at most
`MaxRetryAfter` each (5xx/dial backoffs are well under a second) — so with the
defaults a call returns within about `3 × 30 s + 2 × 60 s`, and a shorter
context deadline cuts it earlier.

> **Idempotency caveat.** The API has no idempotency key yet. A 5xx or a
> connection reset can occur *after* the server queued the message, so a
> retry could send it twice. That is why 5xx retries are opt-in and
> mid-request failures are never retried; if you add your own retry loop
> around `Send`, keep this in mind. When an idempotency key ships, the SDK
> will use it and this caveat goes away.

## Options

| Option | Default | Notes |
|---|---|---|
| `WithBaseURL(url)` | `DefaultBaseURL` = `https://api.your-email.eu` (optional) | **Self-hosted instances only.** The Endpoint the console showed (`https://smtp.example.com/api/v1/messages`) or the API origin (`https://smtp.example.com`); `/api`, `/api/v1` and trailing slashes are understood too. `client.Endpoint()` returns the resolved URL. A blank value or a URL carrying `user:pass@` is refused: the API key is the only credential. |
| `WithHTTPClient(*http.Client)` | `&http.Client{}` | Bring your own transport, TLS config, proxy. Used through a shallow copy that never follows redirects unless you set `CheckRedirect` yourself. |
| `WithTimeout(d)` | `30s` | Per attempt. A shorter context deadline wins. |
| `WithMaxRetries(n)` | `2` | Retries after the first attempt. `0` disables retries. |
| `WithMaxRetryAfter(d)` | `60s` | Longest `Retry-After` to wait out; a 429 asking for more is returned at once with `RetryAfter` set. |
| `WithRetryOnServerError(bool)` | `false` | Also retry 5xx. Read the idempotency caveat first. |
| `WithUserAgentSuffix(s)` | `""` | Appended to `smtp-manager-sdk-go/<version>`. CR/LF make `New` fail. |

## Limits

The API enforces these per message and answers with a 422 naming the field.
They are exported as constants for sizing payloads; the SDK does not check
them locally. Three of them are **defaults of a stock instance** that an
operator can change per deployment; the rest are fixed by the API.

| Constant | Value | Applies to | Fixed? |
|---|---|---|---|
| `MaxRecipients` | 50 | `To` + `Cc` + `Bcc`, no duplicates | fixed |
| `MaxAttachments` | 10 | `Attachments` | stock default |
| `MaxAttachmentsBytes` | 5242880 | decoded size of all attachments combined (5 MiB) | stock default |
| `MaxTags` | 10 | `Tags`, each `^[A-Za-z0-9_.:-]{1,64}$` | fixed |
| `MaxMetadataKeys` | 20 | `Metadata`, keys `^[A-Za-z0-9_.-]{1,64}$`, values up to 512 chars | fixed |
| `MaxBodyLength` | 1048576 | `Text` and `HTML`, each (1 MiB) | stock default |
| `MaxHeaders` | 25 | `Headers` | fixed |
| `MaxSubjectLength` | 998 | `Subject` (required) | fixed |

The whole request body is additionally capped at 12 MB by the proxy (`CodePayloadTooLarge`).

## Where the base URL and key come from

Create an API key in the SMTP Manager console under your organization's API
keys. The console shows the **Endpoint** (`https://host/api/v1/messages`) and
the **full key** (`smtpk_xxxxxxxxxx.<secret>`) exactly once, at creation;
neither the console nor the API can show the secret again. Store the key in
your secret manager and pass it to `New`. The key must have the `send`
ability, otherwise every call returns `CodePermission`.

- **Hosted platform** (`https://api.your-email.eu`): nothing else to
  configure — `New(apiKey)` already points there (`DefaultBaseURL`).
- **Self-hosted instance**: pass the Endpoint the console showed, or the API
  origin, to `WithBaseURL` — both resolve to the same URL.

`New` checks only that the key starts with `smtpk_`; that is the one server
rule mirrored locally, because a key of the wrong shape is a configuration
mistake (the wrong secret pasted) rather than user input. Printing a
`*Client` with `%v` or `%#v` shows the key redacted (`smtpk_abcd…`), so it is
safe to log.

## Testing

```bash
make test              # unit tests with -race and coverage (httptest only, no network)
make lint              # golangci-lint
make coverage          # coverage.html from the last test run
```

The integration test (`integration_test.go`, build tag `integration`) sends
two real requests: a full-composition message that must be accepted, and a
message from an unknown alias that must be refused with `Errors["from"]`. It
skips itself unless the three required variables are set:

```bash
export SMTP_MANAGER_API_KEY=smtpk_xxxxxxxxxx.secret
export SMTP_MANAGER_FROM=noreply@your-verified-domain.example   # active alias
export SMTP_MANAGER_TO=you@example.org                          # a mailbox you control
export SMTP_MANAGER_BASE_URL=https://smtp.example.com           # optional: self-hosted only
make test-integration
```

Without `SMTP_MANAGER_BASE_URL` the test sends through the hosted platform
(`DefaultBaseURL`). In CI the integration job is `workflow_dispatch` only
(Actions → CI → Run workflow) and reads the same values from repository
secrets; it never runs on pushes or pull requests, because it sends real mail.

## Contributing

Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `chore:` ...). [release-please](https://github.com/googleapis/release-please)
turns them into the changelog and version tags on `main`, so a `feat:`
commit bumps the minor version and a `fix:` the patch. Please keep
`make lint` and `make test` green and add a test for every behaviour change.

## License

[MIT](LICENSE) © DevHat
