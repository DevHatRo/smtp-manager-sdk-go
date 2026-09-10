package smtpmanager_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	smtpmanager "github.com/DevHatRo/smtp-manager-sdk-go"
)

// ExampleClient_Send sends a message against a stub server so it can run
// anywhere; replace the base URL and key with the values from your console.
func ExampleClient_Send() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "01J9EXAMPLE")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"data":{"uuid":"3f1c2b0e-4d5a-4f6b-8c7d-9e0f1a2b3c4d","status":"queued",
			"source":"api","from":"noreply@example.com","from_name":"Example","to":["Ann <ann@example.org>"],
			"cc":[],"bcc":[],"reply_to":null,"subject":"Welcome","message_id":"<3f1c2b0e@example.com>",
			"tags":["welcome"],"metadata":{"user_id":"42"},"attachments":[],"attempts":0,"node_id":null,
			"queue_id":null,"error":null,"sent_at":null,"delivered_at":null,"retry_of":null,"retried_as":null,
			"created_at":"2026-09-10T10:00:00Z","updated_at":"2026-09-10T10:00:00Z"}}`)
	}))
	defer srv.Close()

	client, err := smtpmanager.New("smtpk_example.secret",
		smtpmanager.WithBaseURL(srv.URL),
		smtpmanager.WithTimeout(10*time.Second),
	)
	if err != nil {
		fmt.Println("configuration error:", err)
		return
	}

	sent, err := client.Send(context.Background(), &smtpmanager.Message{
		From:     smtpmanager.Address{Email: "noreply@example.com", Name: "Example"},
		To:       []smtpmanager.Address{{Email: "ann@example.org", Name: "Ann"}},
		Subject:  "Welcome",
		Text:     "Thanks for signing up.",
		Tags:     []string{"welcome"},
		Metadata: map[string]string{"user_id": "42"},
	})
	if err != nil {
		var apiErr *smtpmanager.Error
		if errors.As(err, &apiErr) && apiErr.Code == smtpmanager.CodeValidation {
			fmt.Println("invalid message:", apiErr.Errors)
			return
		}
		fmt.Println("send failed:", err)
		return
	}

	fmt.Println(sent.Status, sent.MessageID, sent.RequestID)
	// Output: queued <3f1c2b0e@example.com> 01J9EXAMPLE
}
