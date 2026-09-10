// Package smtpmanager is a zero-dependency Go SDK for the SMTP Manager
// transactional send API (https://github.com/DevHatRo).
//
// The API exposes a single endpoint, POST /api/v1/messages, which accepts a
// fully composed message and queues it for delivery. This package wraps that
// endpoint with typed request and response structures, typed errors, and a
// small retry policy for rate limits and dial failures.
//
// # Quick Start
//
//	// WithBaseURL takes the Endpoint the console showed when the key was
//	// created, or just the API origin; both resolve to /api/v1/messages.
//	client, err := smtpmanager.New(os.Getenv("SMTP_MANAGER_API_KEY"),
//	    smtpmanager.WithBaseURL(os.Getenv("SMTP_MANAGER_BASE_URL")),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	sent, err := client.Send(ctx, &smtpmanager.Message{
//	    From:    smtpmanager.Address{Email: "noreply@example.com", Name: "Example"},
//	    To:      []smtpmanager.Address{{Email: "user@example.org"}},
//	    Subject: "Hello",
//	    Text:    "Hello from SMTP Manager.",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Println(sent.UUID, sent.Status)
//
// Errors returned by the client are *Error values; use errors.As or the
// IsValidation, IsRateLimited and IsAuthentication helpers to classify them.
package smtpmanager
