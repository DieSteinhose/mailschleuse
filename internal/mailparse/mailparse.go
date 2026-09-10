// Package mailparse turns a raw RFC 5322 message into the structured view the
// web UI renders: decoded headers, a plain text body, an HTML body and the
// attachment list. It is deliberately forgiving - a development mail sink has
// to show whatever an application actually produced, including malformed MIME.
package mailparse

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// Address is a single decoded mail address.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// String renders the address the way a mail client would display it.
func (a Address) String() string {
	if a.Name == "" {
		return a.Address
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Address)
}

// Header is one header field in the order it appeared in the message.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Attachment is a MIME part that is not part of the displayed body.
type Attachment struct {
	PartID      string `json:"partId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	ContentID   string `json:"contentId,omitempty"`
	Inline      bool   `json:"inline"`
	Size        int    `json:"size"`

	Content []byte `json:"-"`
}

// Message is the parsed representation of a raw mail.
type Message struct {
	Headers     []Header     `json:"headers"`
	Subject     string       `json:"subject"`
	Date        time.Time    `json:"date"`
	MessageID   string       `json:"messageId,omitempty"`
	From        []Address    `json:"from"`
	To          []Address    `json:"to"`
	Cc          []Address    `json:"cc"`
	Bcc         []Address    `json:"bcc"`
	ReplyTo     []Address    `json:"replyTo"`
	Text        string       `json:"text"`
	HTML        string       `json:"html"`
	Attachments []Attachment `json:"attachments"`
}

// maxDepth caps MIME nesting so a hand-crafted message cannot make the parser
// recurse without bound.
const maxDepth = 12

// Parse decodes raw into a Message. It never fails on malformed input; parts it
// cannot understand end up as attachments or as the raw body text.
func Parse(raw []byte) *Message {
	headerBytes, body := splitMessage(raw)
	msg := &Message{
		Headers:     parseHeaders(headerBytes),
		Attachments: []Attachment{},
		From:        []Address{},
		To:          []Address{},
		Cc:          []Address{},
		Bcc:         []Address{},
		ReplyTo:     []Address{},
	}

	msg.Subject = DecodeWord(msg.header("Subject"))
	msg.MessageID = strings.TrimSpace(msg.header("Message-ID"))
	msg.Date = parseDate(msg.header("Date"))
	msg.From = ParseAddressList(msg.header("From"))
	msg.To = ParseAddressList(msg.header("To"))
	msg.Cc = ParseAddressList(msg.header("Cc"))
	msg.Bcc = ParseAddressList(msg.header("Bcc"))
	msg.ReplyTo = ParseAddressList(msg.header("Reply-To"))

	if len(msg.Headers) == 0 {
		// Nothing that looks like a header block: treat the whole input as body.
		body = raw
	}
	msg.walk(headerMap(msg.Headers), body, "1", 0)

	// A message with neither text nor HTML part still deserves a readable body.
	if msg.Text == "" && msg.HTML == "" && len(msg.Attachments) == 0 {
		msg.Text = toUTF8(body, "")
	}
	return msg
}

// header returns the first value of the named header, case-insensitively.
func (m *Message) header(name string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// headerMap reduces the ordered header list to the lookup map the MIME walker
// needs.
func headerMap(headers []Header) map[string]string {
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		key := strings.ToLower(h.Name)
		if _, exists := out[key]; !exists {
			out[key] = h.Value
		}
	}
	return out
}

// walk decodes one MIME entity, recursing into multipart containers.
func (m *Message) walk(headers map[string]string, body []byte, partID string, depth int) {
	if depth > maxDepth {
		return
	}

	ctype, params := parseMediaType(headers["content-type"])
	disposition, dispParams := parseMediaType(headers["content-disposition"])

	if strings.HasPrefix(ctype, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			m.addAttachment(headers, body, partID, ctype, disposition, dispParams)
			return
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		index := 0
		for {
			part, err := reader.NextRawPart()
			if err != nil {
				if !errors.Is(err, io.EOF) && index == 0 {
					// Broken container: keep the raw bytes visible.
					m.appendText(toUTF8(body, params["charset"]))
				}
				return
			}
			index++
			content, _ := io.ReadAll(part)
			sub := make(map[string]string, len(part.Header))
			for k, v := range part.Header {
				if len(v) > 0 {
					sub[strings.ToLower(k)] = v[0]
				}
			}
			m.walk(sub, content, fmt.Sprintf("%s.%d", partID, index), depth+1)
			part.Close()
		}
	}

	decoded, err := decodeTransfer(body, headers["content-transfer-encoding"])
	if err != nil {
		decoded = body
	}

	filename := DecodeWord(firstNonEmpty(dispParams["filename"], params["name"]))
	isAttachment := disposition == "attachment" || filename != "" ||
		(ctype != "" && !strings.HasPrefix(ctype, "text/") && !strings.HasPrefix(ctype, "message/"))

	switch {
	case !isAttachment && (ctype == "" || ctype == "text/plain"):
		m.appendText(toUTF8(decoded, params["charset"]))
	case !isAttachment && ctype == "text/html":
		m.appendHTML(toUTF8(decoded, params["charset"]))
	default:
		m.addAttachment(headers, decoded, partID, ctype, disposition, dispParams)
	}
}

func (m *Message) appendText(s string) {
	if m.Text != "" {
		m.Text += "\n"
	}
	m.Text += s
}

func (m *Message) appendHTML(s string) {
	if m.HTML != "" {
		m.HTML += "\n"
	}
	m.HTML += s
}

func (m *Message) addAttachment(headers map[string]string, content []byte, partID, ctype, disposition string, dispParams map[string]string) {
	_, params := parseMediaType(headers["content-type"])
	filename := DecodeWord(firstNonEmpty(dispParams["filename"], params["name"]))
	if filename == "" {
		filename = fmt.Sprintf("part-%s%s", partID, extensionFor(ctype))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	m.Attachments = append(m.Attachments, Attachment{
		PartID:      partID,
		Filename:    SanitizeFilename(filename),
		ContentType: ctype,
		ContentID:   strings.Trim(strings.TrimSpace(headers["content-id"]), "<>"),
		Inline:      disposition == "inline",
		Size:        len(content),
		Content:     content,
	})
}

// splitMessage separates the header block from the body at the first empty line.
func splitMessage(raw []byte) ([]byte, []byte) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i], raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i], raw[i+2:]
	}
	return raw, nil
}

// parseHeaders unfolds and splits the header block, preserving field order.
func parseHeaders(block []byte) []Header {
	var headers []Header
	lines := strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(headers) > 0 {
			headers[len(headers)-1].Value += " " + strings.TrimSpace(line)
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		headers = append(headers, Header{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	return headers
}

// parseMediaType parses a Content-Type style header, tolerating broken
// parameters such as an unterminated charset quote.
func parseMediaType(raw string) (string, map[string]string) {
	if strings.TrimSpace(raw) == "" {
		return "", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(raw)
	if err != nil {
		base, _, _ := strings.Cut(raw, ";")
		return strings.ToLower(strings.TrimSpace(base)), map[string]string{}
	}
	return strings.ToLower(mt), params
}

func decodeTransfer(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		// Mail generators wrap base64 in ways Go's strict decoder rejects, so
		// drop all whitespace and decode without padding requirements.
		cleaned := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		out, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(
			strings.TrimRight(string(cleaned), "="))
		if err != nil {
			return nil, err
		}
		return out, nil
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
	default:
		return body, nil
	}
}

// DecodeWord decodes RFC 2047 encoded-words, falling back to the raw value.
func DecodeWord(raw string) string {
	if raw == "" {
		return ""
	}
	dec := &mime.WordDecoder{CharsetReader: charsetReader}
	out, err := dec.DecodeHeader(raw)
	if err != nil {
		return raw
	}
	return out
}

// ParseAddressList decodes an address header into individual addresses. Invalid
// lists still yield something displayable rather than an empty result.
func ParseAddressList(raw string) []Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []Address{}
	}
	parser := mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: charsetReader}}
	list, err := parser.ParseList(raw)
	if err != nil {
		return []Address{{Address: DecodeWord(raw)}}
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		out = append(out, Address{Name: a.Name, Address: a.Address})
	}
	return out
}

func parseDate(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if t, err := mail.ParseDate(raw); err == nil {
		return t
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func extensionFor(ctype string) string {
	if exts, err := mime.ExtensionsByType(ctype); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// SanitizeFilename strips path separators and control characters so a message
// can never influence where an attachment is written or how it is served.
func SanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f:
			return -1
		case r == '/', r == '\\', r == ':':
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || strings.Trim(name, ".") == "" {
		return "attachment.bin"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// toUTF8 converts a decoded part body to UTF-8. Only the charsets a developer
// realistically hits are supported; anything else falls back to a byte-wise
// mapping so the UI never has to render invalid UTF-8.
func toUTF8(body []byte, charset string) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		if utf8.Valid(body) {
			return string(body)
		}
		return strings.ToValidUTF8(string(body), "�")
	case "iso-8859-1", "latin1", "iso8859-1", "iso-8859-15", "cp1252", "windows-1252":
		return decodeLatin(body, strings.Contains(strings.ToLower(charset), "1252"))
	default:
		if utf8.Valid(body) {
			return string(body)
		}
		return decodeLatin(body, false)
	}
}

// charsetReader lets mime.WordDecoder handle the same charsets as toUTF8.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	body, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(toUTF8(body, charset)), nil
}

// cp1252High maps the 0x80-0x9F range that Windows-1252 uses for printable
// characters and ISO-8859-1 leaves as control codes. Slots that Windows-1252
// leaves undefined map to U+FFFD.
var cp1252High = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
}

func decodeLatin(body []byte, windows bool) string {
	var b strings.Builder
	b.Grow(len(body))
	for _, c := range body {
		if windows && c >= 0x80 && c <= 0x9F {
			b.WriteRune(cp1252High[c-0x80])
			continue
		}
		b.WriteRune(rune(c))
	}
	return b.String()
}
