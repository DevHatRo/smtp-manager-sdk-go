package smtpmanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAddress_String(t *testing.T) {
	cases := []struct {
		addr Address
		want string
	}{
		{Address{Email: "a@example.com"}, "a@example.com"},
		{Address{Email: "a@example.com", Name: "Ann Example"}, "Ann Example <a@example.com>"},
		{Address{}, ""},
	}
	for _, tc := range cases {
		if got := tc.addr.String(); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestAddress_Validate(t *testing.T) {
	ok := []string{"", "Ann", "Ann Example", "O'Brien, Ann", `"Quoted"`, "Ünïcödé 名前"}
	for _, n := range ok {
		if err := (Address{Email: "a@b", Name: n}).validate("from"); err != nil {
			t.Errorf("%q: unexpected %v", n, err)
		}
	}
	bad := []string{"a<b", "a>b", "a\rb", "a\nb", "<a@b>"}
	for _, n := range bad {
		err := (Address{Email: "a@b", Name: n}).validate("to[0]")
		e, isErr := AsError(err)
		if !isErr || e.Code != CodeInvalidInput {
			t.Errorf("%q: got %v", n, err)
			continue
		}
		if !strings.HasPrefix(e.Message, "to[0]: display name") {
			t.Errorf("%q: message should name the field: %q", n, e.Message)
		}
	}
	// The same characters break the rendering when they are in the email.
	for _, email := range []string{"<a@b>", "a@b>", "a\r@b", "a@b\n"} {
		err := (Address{Email: email}).validate("from")
		e, isErr := AsError(err)
		if !isErr || e.Code != CodeInvalidInput || !strings.HasPrefix(e.Message, "from: email") {
			t.Errorf("%q: got %v", email, err)
		}
	}
	// Odd but renderable emails are left to the server.
	for _, email := range []string{"", "not-an-email", "a b@c", "ünï@example.com"} {
		if err := (Address{Email: email}).validate("from"); err != nil {
			t.Errorf("%q: unexpected %v", email, err)
		}
	}
}

func TestMessage_ToWire(t *testing.T) {
	m := &Message{
		From:    Address{Email: "a@b", Name: "A"},
		To:      []Address{{Email: "c@d"}},
		Subject: "s",
		Text:    "t",
	}
	w, err := m.toWire()
	if err != nil {
		t.Fatal(err)
	}
	if w.From != "A <a@b>" || len(w.To) != 1 || w.To[0] != "c@d" || w.ReplyTo != "" || w.Cc != nil || w.Bcc != nil || w.Attachments != nil {
		t.Errorf("%+v", w)
	}

	m.ReplyTo = Address{Name: "ignored without an email"}
	if w, _ = m.toWire(); w.ReplyTo != "" {
		t.Errorf("ReplyTo with empty Email should be omitted, got %q", w.ReplyTo)
	}
	m.ReplyTo = Address{Email: "r@s", Name: "R"}
	if w, _ = m.toWire(); w.ReplyTo != "R <r@s>" {
		t.Errorf("ReplyTo = %q", w.ReplyTo)
	}

	var nilMsg *Message
	if _, err := nilMsg.toWire(); !isCode(err, CodeInvalidInput) {
		t.Errorf("nil message: %v", err)
	}

	// JSON of an empty attachment content is an empty string, not null.
	m.Attachments = []Attachment{{Filename: "empty.bin", ContentType: "application/octet-stream"}}
	w, _ = m.toWire()
	b, _ := json.Marshal(w)
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("empty attachment: %s", b)
	}
}

func isCode(err error, code string) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}

func TestAttachmentFromFile(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		content  string
		wantType string
	}{
		{"notes.txt", "hello", "text/plain"},
		{"report.pdf", "%PDF-1.4", "application/pdf"},
		{"image.png", "\x89PNG", "image/png"},
		{"data.unknownext", "raw", "application/octet-stream"},
		{"noext", "raw", "application/octet-stream"},
	}
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := AttachmentFromFile(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if a.Filename != tc.name {
			t.Errorf("%s: Filename = %q", tc.name, a.Filename)
		}
		// mime.TypeByExtension may append a charset parameter for text types.
		if !strings.HasPrefix(a.ContentType, tc.wantType) {
			t.Errorf("%s: ContentType = %q, want prefix %q", tc.name, a.ContentType, tc.wantType)
		}
		if string(a.Content) != tc.content {
			t.Errorf("%s: Content = %q", tc.name, a.Content)
		}
		if a.ContentID != "" {
			t.Errorf("%s: ContentID should be empty", tc.name)
		}
	}

	if _, err := AttachmentFromFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := AttachmentFromFile(dir); err == nil {
		t.Error("directory should error")
	}
}

// TestLimits pins the README's limits table to the exported constants, so
// the two cannot drift apart silently.
func TestLimits(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	limits := map[string]int{
		"MaxRecipients":       MaxRecipients,
		"MaxAttachments":      MaxAttachments,
		"MaxAttachmentsBytes": MaxAttachmentsBytes,
		"MaxTags":             MaxTags,
		"MaxMetadataKeys":     MaxMetadataKeys,
		"MaxBodyLength":       MaxBodyLength,
		"MaxHeaders":          MaxHeaders,
		"MaxSubjectLength":    MaxSubjectLength,
	}
	for name, value := range limits {
		row := "| `" + name + "` | " + strconv.Itoa(value) + " |"
		if !strings.Contains(string(readme), row) {
			t.Errorf("README limits table has no row starting %q", row)
		}
	}
}

func TestWireSentMessage_Roundtrip(t *testing.T) {
	var w wireSentMessage
	if err := json.Unmarshal([]byte(`{"uuid":"u","attachments":null,"to":null}`), &w); err != nil {
		t.Fatal(err)
	}
	s := w.toSentMessage()
	if s.UUID != "u" || s.To != nil || s.Attachments != nil || s.NodeID != nil || s.SentAt != nil {
		t.Errorf("%+v", s)
	}
	if deref(nil) != "" {
		t.Error("deref(nil)")
	}
}
