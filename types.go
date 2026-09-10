package smtpmanager

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits the API enforces on a single message, documented here so callers
// can size their payloads. The SDK does not enforce them locally; the server
// does, with an HTTP 422 naming the field.
//
// MaxRecipients, MaxTags, MaxMetadataKeys, MaxHeaders and MaxSubjectLength
// are fixed by the API. MaxAttachments, MaxAttachmentsBytes and MaxBodyLength
// are the defaults of a stock instance and can be raised or lowered per
// deployment by the operator, so a specific instance may differ.
const (
	// MaxRecipients is the maximum number of addresses across To, Cc and Bcc.
	MaxRecipients = 50
	// MaxAttachments is the default maximum number of attachments per message.
	MaxAttachments = 10
	// MaxAttachmentsBytes is the default maximum decoded size of all
	// attachments combined (5 MiB).
	MaxAttachmentsBytes = 5 * 1024 * 1024
	// MaxTags is the maximum number of tags per message.
	MaxTags = 10
	// MaxMetadataKeys is the maximum number of metadata entries per message.
	MaxMetadataKeys = 20
	// MaxBodyLength is the default maximum length in characters of Text and
	// of HTML (1 MiB each).
	MaxBodyLength = 1024 * 1024
	// MaxHeaders is the maximum number of custom headers per message.
	MaxHeaders = 25
	// MaxSubjectLength is the maximum subject length in characters (RFC 5322 line limit).
	MaxSubjectLength = 998
)

// Address is an email address with an optional display name.
type Address struct {
	Email string
	Name  string
}

// String renders the address as "Name <email>" when Name is set, or the bare
// email otherwise. This is the form sent on the wire.
func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

// validate rejects the characters that would break the "Name <email>"
// rendering, in either half. Everything else (address syntax, alias
// ownership, domain verification) is validated by the API so the SDK never
// drifts from the server rules.
func (a Address) validate(field string) error {
	const unsafe = "<>\r\n"
	if strings.ContainsAny(a.Name, unsafe) {
		return &Error{
			Code:    CodeInvalidInput,
			Message: fmt.Sprintf("%s: display name %q must not contain '<', '>', CR or LF", field, a.Name),
		}
	}
	if strings.ContainsAny(a.Email, unsafe) {
		return &Error{
			Code:    CodeInvalidInput,
			Message: fmt.Sprintf("%s: email %q must not contain '<', '>', CR or LF", field, a.Email),
		}
	}
	return nil
}

// Message is a transactional message to send.
//
// From must be an active alias on a verified domain in the API key's
// organization. At least one of Text and HTML is required.
type Message struct {
	From Address
	To   []Address
	Cc   []Address
	Bcc  []Address
	// ReplyTo is optional; leave Email empty to omit it.
	ReplyTo Address
	Subject string
	Text    string
	HTML    string
	// Headers are additional message headers. Addressing, Received,
	// Message-ID and DKIM-* headers are refused by the API.
	Headers     map[string]string
	Attachments []Attachment
	// Tags label the message for filtering; each must match ^[A-Za-z0-9_.:-]{1,64}$.
	Tags []string
	// Metadata is free-form key/value data echoed back on the message. Keys
	// must match ^[A-Za-z0-9_.-]{1,64}$ and values are limited to 512
	// characters. Key order is not preserved server-side.
	Metadata map[string]string
}

// Attachment is a file attached to a Message. The SDK base64-encodes Content.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
	// ContentID, when set, marks the attachment as an inline part that HTML
	// can reference as "cid:<ContentID>". It must look like "local@domain".
	ContentID string
}

// AttachmentFromFile reads path into an Attachment. The filename is the base
// name of path and the content type is derived from the extension, falling
// back to "application/octet-stream".
func AttachmentFromFile(path string) (Attachment, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, err
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return Attachment{
		Filename:    filepath.Base(path),
		ContentType: ct,
		Content:     content,
	}, nil
}

// SentMessage is the message resource returned by the API once a message has
// been accepted (HTTP 202).
type SentMessage struct {
	UUID string
	// Status is one of "queued", "sending", "sent" or "failed".
	Status string
	// Source is "api" for messages submitted through this endpoint.
	Source   string
	From     string
	FromName string
	To       []string
	Cc       []string
	Bcc      []string
	ReplyTo  string
	Subject  string
	// MessageID is the RFC 5322 Message-ID assigned by the platform, in the
	// form "<uuid@domain>".
	MessageID   string
	Tags        []string
	Metadata    map[string]string
	Attachments []SentAttachment
	// Attempts is the number of delivery attempts made so far.
	Attempts int
	// NodeID is the sending node, once one has been assigned.
	NodeID *int
	// QueueID is the Postfix queue identifier, once handed to a node.
	QueueID string
	// Error is the last delivery error, if any.
	Error       string
	SentAt      *time.Time
	DeliveredAt *time.Time
	// RetryOf is the UUID of the message this one retries, if any.
	RetryOf string
	// RetriedAs is the UUID of the message that retried this one, if any.
	RetriedAs string
	CreatedAt time.Time
	UpdatedAt time.Time
	// RequestID is the X-Request-Id of the request that created the message.
	RequestID string
}

// SentAttachment describes an attachment as stored by the API.
type SentAttachment struct {
	Filename    string
	ContentType string
	ContentID   string
	// Size is the decoded size in bytes.
	Size int
}

// wireMessage is the JSON request body.
type wireMessage struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text,omitempty"`
	HTML        string            `json:"html,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []wireAttachment  `json:"attachments,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type wireAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
	ContentID   string `json:"content_id,omitempty"`
}

// toWire validates the addresses and builds the request body.
func (m *Message) toWire() (*wireMessage, error) {
	if m == nil {
		return nil, &Error{Code: CodeInvalidInput, Message: "message must not be nil"}
	}
	if err := m.From.validate("from"); err != nil {
		return nil, err
	}
	w := &wireMessage{
		From:     m.From.String(),
		Subject:  m.Subject,
		Text:     m.Text,
		HTML:     m.HTML,
		Headers:  m.Headers,
		Tags:     m.Tags,
		Metadata: m.Metadata,
	}
	var err error
	if w.To, err = renderAddresses("to", m.To); err != nil {
		return nil, err
	}
	if w.Cc, err = renderAddresses("cc", m.Cc); err != nil {
		return nil, err
	}
	if w.Bcc, err = renderAddresses("bcc", m.Bcc); err != nil {
		return nil, err
	}
	if m.ReplyTo.Email != "" {
		if err := m.ReplyTo.validate("reply_to"); err != nil {
			return nil, err
		}
		w.ReplyTo = m.ReplyTo.String()
	}
	if len(m.Attachments) > 0 {
		w.Attachments = make([]wireAttachment, len(m.Attachments))
		for i, a := range m.Attachments {
			w.Attachments[i] = wireAttachment{
				Filename:    a.Filename,
				ContentType: a.ContentType,
				Content:     base64.StdEncoding.EncodeToString(a.Content),
				ContentID:   a.ContentID,
			}
		}
	}
	return w, nil
}

func renderAddresses(field string, addrs []Address) ([]string, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		if err := a.validate(fmt.Sprintf("%s[%d]", field, i)); err != nil {
			return nil, err
		}
		out[i] = a.String()
	}
	return out, nil
}

// wireSentMessage is the "data" object of a 202 response.
type wireSentMessage struct {
	UUID        string               `json:"uuid"`
	Status      string               `json:"status"`
	Source      string               `json:"source"`
	From        string               `json:"from"`
	FromName    *string              `json:"from_name"`
	To          []string             `json:"to"`
	Cc          []string             `json:"cc"`
	Bcc         []string             `json:"bcc"`
	ReplyTo     *string              `json:"reply_to"`
	Subject     string               `json:"subject"`
	MessageID   *string              `json:"message_id"`
	Tags        []string             `json:"tags"`
	Metadata    map[string]string    `json:"metadata"`
	Attachments []wireSentAttachment `json:"attachments"`
	Attempts    int                  `json:"attempts"`
	NodeID      *int                 `json:"node_id"`
	QueueID     *string              `json:"queue_id"`
	Error       *string              `json:"error"`
	SentAt      *time.Time           `json:"sent_at"`
	DeliveredAt *time.Time           `json:"delivered_at"`
	RetryOf     *string              `json:"retry_of"`
	RetriedAs   *string              `json:"retried_as"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

type wireSentAttachment struct {
	Filename    string  `json:"filename"`
	ContentType string  `json:"content_type"`
	ContentID   *string `json:"content_id"`
	Size        int     `json:"size"`
}

func (w *wireSentMessage) toSentMessage() *SentMessage {
	s := &SentMessage{
		UUID:        w.UUID,
		Status:      w.Status,
		Source:      w.Source,
		From:        w.From,
		FromName:    deref(w.FromName),
		To:          w.To,
		Cc:          w.Cc,
		Bcc:         w.Bcc,
		ReplyTo:     deref(w.ReplyTo),
		Subject:     w.Subject,
		MessageID:   deref(w.MessageID),
		Tags:        w.Tags,
		Metadata:    w.Metadata,
		Attempts:    w.Attempts,
		NodeID:      w.NodeID,
		QueueID:     deref(w.QueueID),
		Error:       deref(w.Error),
		SentAt:      w.SentAt,
		DeliveredAt: w.DeliveredAt,
		RetryOf:     deref(w.RetryOf),
		RetriedAs:   deref(w.RetriedAs),
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
	}
	if len(w.Attachments) > 0 {
		s.Attachments = make([]SentAttachment, len(w.Attachments))
		for i, a := range w.Attachments {
			s.Attachments[i] = SentAttachment{
				Filename:    a.Filename,
				ContentType: a.ContentType,
				ContentID:   deref(a.ContentID),
				Size:        a.Size,
			}
		}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
