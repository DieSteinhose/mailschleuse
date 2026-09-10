package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
)

// ComposeRequest is the JSON body accepted by POST /api/messages. It describes
// a mail the way a developer thinks about it; turning that into valid MIME is
// this file's job.
type ComposeRequest struct {
	Mailbox     string              `json:"mailbox"`
	From        string              `json:"from"`
	To          []string            `json:"to"`
	Cc          []string            `json:"cc"`
	Bcc         []string            `json:"bcc"`
	ReplyTo     string              `json:"replyTo"`
	Subject     string              `json:"subject"`
	Text        string              `json:"text"`
	HTML        string              `json:"html"`
	Headers     map[string]string   `json:"headers"`
	Attachments []ComposeAttachment `json:"attachments"`
}

// ComposeAttachment carries a file as base64 so the whole request stays JSON.
type ComposeAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// headersSetByBuilder are generated from the structured fields and must not be
// overridden through the free-form headers map.
var headersSetByBuilder = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true,
	"date": true, "message-id": true, "reply-to": true,
	"mime-version": true, "content-type": true, "content-transfer-encoding": true,
}

// Build renders the request as an RFC 5322 message with CRLF line endings.
func (c *ComposeRequest) Build(hostname string) ([]byte, error) {
	from := strings.TrimSpace(c.From)
	if from == "" {
		return nil, errors.New("field \"from\" is required")
	}
	recipients := cleanList(c.To)
	if len(recipients) == 0 {
		return nil, errors.New("at least one recipient in \"to\" is required")
	}
	if strings.TrimSpace(c.Text) == "" && strings.TrimSpace(c.HTML) == "" {
		return nil, errors.New("either \"text\" or \"html\" must be set")
	}

	var head bytes.Buffer
	writeHeader(&head, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(&head, "Message-ID", fmt.Sprintf("<%s@%s>", randomToken(), hostname))
	writeHeader(&head, "From", encodeAddressHeader(from))
	writeHeader(&head, "To", encodeAddressHeader(strings.Join(recipients, ", ")))
	if cc := cleanList(c.Cc); len(cc) > 0 {
		writeHeader(&head, "Cc", encodeAddressHeader(strings.Join(cc, ", ")))
	}
	if bcc := cleanList(c.Bcc); len(bcc) > 0 {
		writeHeader(&head, "Bcc", encodeAddressHeader(strings.Join(bcc, ", ")))
	}
	if replyTo := strings.TrimSpace(c.ReplyTo); replyTo != "" {
		writeHeader(&head, "Reply-To", encodeAddressHeader(replyTo))
	}
	writeHeader(&head, "Subject", mime.QEncoding.Encode("utf-8", c.Subject))
	for name, value := range c.Headers {
		name = strings.TrimSpace(name)
		if name == "" || headersSetByBuilder[strings.ToLower(name)] {
			continue
		}
		writeHeader(&head, name, sanitizeHeaderValue(value))
	}
	writeHeader(&head, "MIME-Version", "1.0")

	body, contentHeaders, err := c.buildBody()
	if err != nil {
		return nil, err
	}
	for _, h := range contentHeaders {
		writeHeader(&head, h.name, h.value)
	}
	head.WriteString("\r\n")
	return append(head.Bytes(), body...), nil
}

type headerPair struct{ name, value string }

// buildBody assembles the MIME structure: multipart/mixed when attachments are
// present, multipart/alternative for text plus HTML, a single part otherwise.
func (c *ComposeRequest) buildBody() ([]byte, []headerPair, error) {
	attachments, err := c.decodeAttachments()
	if err != nil {
		return nil, nil, err
	}

	if len(attachments) == 0 {
		return c.buildContent()
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(randomBoundary()); err != nil {
		return nil, nil, err
	}

	content, contentHeaders, err := c.buildContent()
	if err != nil {
		return nil, nil, err
	}
	partHeaders := textproto.MIMEHeader{}
	for _, h := range contentHeaders {
		partHeaders.Set(h.name, h.value)
	}
	part, err := mw.CreatePart(partHeaders)
	if err != nil {
		return nil, nil, err
	}
	if _, err := part.Write(content); err != nil {
		return nil, nil, err
	}

	for _, a := range attachments {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", mime.FormatMediaType(a.contentType, map[string]string{"name": a.filename}))
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.filename}))
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, nil, err
		}
		if _, err := w.Write(wrapBase64(a.content)); err != nil {
			return nil, nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, nil, err
	}
	return normalizeCRLF(buf.Bytes()), []headerPair{
		{"Content-Type", fmt.Sprintf("multipart/mixed; boundary=%q", mw.Boundary())},
	}, nil
}

// buildContent renders just the readable body of the message.
func (c *ComposeRequest) buildContent() ([]byte, []headerPair, error) {
	text, html := strings.TrimSpace(c.Text), strings.TrimSpace(c.HTML)

	switch {
	case html == "":
		return encodeQP(c.Text), []headerPair{
			{"Content-Type", "text/plain; charset=utf-8"},
			{"Content-Transfer-Encoding", "quoted-printable"},
		}, nil
	case text == "":
		return encodeQP(c.HTML), []headerPair{
			{"Content-Type", "text/html; charset=utf-8"},
			{"Content-Transfer-Encoding", "quoted-printable"},
		}, nil
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(randomBoundary()); err != nil {
		return nil, nil, err
	}
	for _, part := range []struct{ ctype, content string }{
		{"text/plain; charset=utf-8", c.Text},
		{"text/html; charset=utf-8", c.HTML},
	} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", part.ctype)
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, nil, err
		}
		if _, err := w.Write(encodeQP(part.content)); err != nil {
			return nil, nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, nil, err
	}
	return normalizeCRLF(buf.Bytes()), []headerPair{
		{"Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", mw.Boundary())},
	}, nil
}

type decodedAttachment struct {
	filename    string
	contentType string
	content     []byte
}

func (c *ComposeRequest) decodeAttachments() ([]decodedAttachment, error) {
	out := make([]decodedAttachment, 0, len(c.Attachments))
	for i, a := range c.Attachments {
		// Browsers hand out data: URLs; accept them as well as bare base64.
		payload := a.Content
		if idx := strings.Index(payload, ";base64,"); idx >= 0 {
			payload = payload[idx+len(";base64,"):]
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
		if err != nil {
			return nil, fmt.Errorf("attachment %d: content is not valid base64", i+1)
		}
		ctype := strings.TrimSpace(a.ContentType)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		filename := strings.TrimSpace(a.Filename)
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d.bin", i+1)
		}
		out = append(out, decodedAttachment{
			filename:    sanitizeHeaderValue(filename),
			contentType: sanitizeHeaderValue(ctype),
			content:     raw,
		})
	}
	return out, nil
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// sanitizeHeaderValue removes CR and LF so a composed value cannot inject
// additional header fields.
func sanitizeHeaderValue(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}

// encodeAddressHeader keeps plain addresses as they are and RFC 2047 encodes
// only the display name, which is the part that may hold non-ASCII text.
func encodeAddressHeader(raw string) string {
	parts := strings.Split(sanitizeHeaderValue(raw), ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		open := strings.LastIndex(part, "<")
		if open <= 0 || !strings.HasSuffix(part, ">") {
			out = append(out, part)
			continue
		}
		name := strings.Trim(strings.TrimSpace(part[:open]), "\"")
		out = append(out, strings.TrimSpace(mime.QEncoding.Encode("utf-8", name)+" "+part[open:]))
	}
	return strings.Join(out, ", ")
}

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, single := range strings.Split(v, ",") {
			if single = sanitizeHeaderValue(single); single != "" {
				out = append(out, single)
			}
		}
	}
	return out
}

func encodeQP(s string) []byte {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	w.Write([]byte(normalizeCRLF([]byte(s))))
	w.Close()
	return buf.Bytes()
}

// wrapBase64 encodes content in 76 character lines as required for mail.
func wrapBase64(content []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(content)
	var buf bytes.Buffer
	for len(encoded) > 76 {
		buf.WriteString(encoded[:76])
		buf.WriteString("\r\n")
		encoded = encoded[76:]
	}
	buf.WriteString(encoded)
	return buf.Bytes()
}

// normalizeCRLF converts any mix of line endings to the CRLF mail requires.
func normalizeCRLF(in []byte) []byte {
	out := bytes.ReplaceAll(in, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
}

func randomToken() string {
	var buf [12]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func randomBoundary() string {
	return "mailschleuse-" + randomToken()
}
