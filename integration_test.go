//go:build integration

package smtpmanager_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	smtpmanager "github.com/DevHatRo/smtp-manager-sdk-go"
)

// TestIntegration_Send talks to a real SMTP Manager instance. It needs:
//
//	SMTP_MANAGER_BASE_URL  e.g. https://smtp.example.com
//	SMTP_MANAGER_API_KEY   an API key with the "send" ability (smtpk_...)
//	SMTP_MANAGER_FROM      an active alias on a verified domain of the key's organization
//	SMTP_MANAGER_TO        a mailbox you control
//
// Run with: make test-integration
func TestIntegration_Send(t *testing.T) {
	baseURL := os.Getenv("SMTP_MANAGER_BASE_URL")
	apiKey := os.Getenv("SMTP_MANAGER_API_KEY")
	from := os.Getenv("SMTP_MANAGER_FROM")
	to := os.Getenv("SMTP_MANAGER_TO")
	if baseURL == "" || apiKey == "" || from == "" || to == "" {
		t.Skip("set SMTP_MANAGER_BASE_URL, SMTP_MANAGER_API_KEY, SMTP_MANAGER_FROM and SMTP_MANAGER_TO to run")
	}
	// The API refuses one address in two lists, so the cc is a distinct
	// address: SMTP_MANAGER_CC when set, otherwise a +cc tag on the recipient.
	cc := os.Getenv("SMTP_MANAGER_CC")
	if cc == "" {
		cc = plusTag(to, "cc")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := smtpmanager.New(apiKey, smtpmanager.WithBaseURL(baseURL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sent, err := client.Send(ctx, &smtpmanager.Message{
		From:    smtpmanager.Address{Email: from, Name: "SDK Integration"},
		To:      []smtpmanager.Address{{Email: to, Name: "Integration Recipient"}},
		Cc:      []smtpmanager.Address{{Email: cc}},
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
		t.Fatalf("Send: %v", err)
	}

	if sent.UUID == "" {
		t.Error("UUID is empty")
	}
	if sent.Status != "queued" && sent.Status != "sending" && sent.Status != "sent" {
		t.Errorf("Status = %q", sent.Status)
	}
	if sent.Source != "api" {
		t.Errorf("Source = %q, want api", sent.Source)
	}
	if !strings.EqualFold(sent.From, from) {
		t.Errorf("From = %q, want %q", sent.From, from)
	}
	if sent.FromName != "SDK Integration" {
		t.Errorf("FromName = %q", sent.FromName)
	}
	if len(sent.To) != 1 || !strings.Contains(sent.To[0], to) {
		t.Errorf("To = %v", sent.To)
	}
	if len(sent.Cc) != 1 {
		t.Errorf("Cc = %v", sent.Cc)
	}
	if !strings.HasPrefix(sent.MessageID, "<") || !strings.HasSuffix(sent.MessageID, ">") {
		t.Errorf("MessageID = %q, want <uuid@domain>", sent.MessageID)
	}
	if len(sent.Tags) != 2 {
		t.Errorf("Tags = %v", sent.Tags)
	}
	if sent.Metadata["suite"] != "integration" || sent.Metadata["sdk"] != "go" {
		t.Errorf("Metadata = %v", sent.Metadata)
	}
	if len(sent.Attachments) != 1 || sent.Attachments[0].Filename != "hello.txt" || sent.Attachments[0].Size != len("hello from the SDK\n") {
		t.Errorf("Attachments = %+v", sent.Attachments)
	}
	if sent.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if sent.RequestID == "" {
		t.Error("RequestID is empty (X-Request-Id header missing)")
	}

	// An unknown sender must be refused with a per-field validation error.
	_, err = client.Send(ctx, &smtpmanager.Message{
		From:    smtpmanager.Address{Email: "nobody@invalid.example"},
		To:      []smtpmanager.Address{{Email: to}},
		Subject: "should be rejected",
		Text:    "never sent",
	})
	if !smtpmanager.IsValidation(err) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	apiErr, _ := smtpmanager.AsError(err)
	if len(apiErr.Errors["from"]) == 0 {
		t.Errorf("expected Errors[\"from\"], got %v", apiErr.Errors)
	}
	if apiErr.RequestID == "" {
		t.Error("validation error carries no request id")
	}
}

// plusTag returns local+tag@domain for local@domain.
func plusTag(email, tag string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email
	}
	return email[:at] + "+" + tag + email[at:]
}
