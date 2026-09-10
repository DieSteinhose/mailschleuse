package mailparse

import (
	"strings"
	"testing"
)

func TestParsePlainMessage(t *testing.T) {
	raw := []byte("From: Jane Doe <jane@example.com>\r\n" +
		"To: support@example.com\r\n" +
		"Subject: Printer offline\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 +0100\r\n" +
		"\r\n" +
		"The printer stopped working.\r\n")

	msg := Parse(raw)
	if msg.Subject != "Printer offline" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if len(msg.From) != 1 || msg.From[0].Address != "jane@example.com" || msg.From[0].Name != "Jane Doe" {
		t.Errorf("from = %+v", msg.From)
	}
	if !strings.Contains(msg.Text, "stopped working") {
		t.Errorf("text = %q", msg.Text)
	}
	if msg.Date.Year() != 2006 {
		t.Errorf("date = %v", msg.Date)
	}
	if len(msg.Headers) != 4 || msg.Headers[0].Name != "From" {
		t.Errorf("headers = %+v", msg.Headers)
	}
}

func TestParseEncodedSubjectAndFoldedHeader(t *testing.T) {
	raw := []byte("Subject: =?UTF-8?Q?Gr=C3=BC=C3=9Fe_aus_M=C3=BCnchen?=\r\n" +
		"X-Long: first part\r\n continued part\r\n" +
		"\r\nbody\r\n")

	msg := Parse(raw)
	if msg.Subject != "Grüße aus München" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if got := msg.header("X-Long"); got != "first part continued part" {
		t.Errorf("folded header = %q", got)
	}
}

func TestParseMultipartAlternative(t *testing.T) {
	raw := []byte("Subject: Multipart\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"Caf=C3=A9 plain\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"PHA+SFRNTCBib2R5PC9wPg==\r\n" +
		"--BOUND--\r\n")

	msg := Parse(raw)
	if !strings.Contains(msg.Text, "Café plain") {
		t.Errorf("text = %q", msg.Text)
	}
	if !strings.Contains(msg.HTML, "<p>HTML body</p>") {
		t.Errorf("html = %q", msg.HTML)
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("expected no attachments, got %d", len(msg.Attachments))
	}
}

func TestParseAttachment(t *testing.T) {
	raw := []byte("Subject: With file\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n\r\nsee attachment\r\n" +
		"--B\r\n" +
		"Content-Type: text/csv; name=\"report.csv\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.csv\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\nYSxiLGMK\r\n" +
		"--B--\r\n")

	msg := Parse(raw)
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(msg.Attachments))
	}
	a := msg.Attachments[0]
	if a.Filename != "report.csv" || a.ContentType != "text/csv" {
		t.Errorf("attachment = %+v", a)
	}
	if string(a.Content) != "a,b,c\n" {
		t.Errorf("attachment content = %q", a.Content)
	}
	if !strings.Contains(msg.Text, "see attachment") {
		t.Errorf("text = %q", msg.Text)
	}
}

func TestParseLatin1Body(t *testing.T) {
	raw := append([]byte("Subject: Latin\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\n"),
		[]byte{'M', 0xFC, 'l', 'l', 'e', 'r'}...)

	if got := Parse(raw).Text; !strings.Contains(got, "Müller") {
		t.Errorf("text = %q, want it to contain Müller", got)
	}
}

func TestParseMalformedMessageStillReadable(t *testing.T) {
	msg := Parse([]byte("this is not a mail at all"))
	if msg.Text == "" {
		t.Error("a body-only blob should still produce readable text")
	}

	broken := Parse([]byte("Subject: broken\r\nContent-Type: multipart/mixed\r\n\r\nno boundary here\r\n"))
	if len(broken.Attachments) == 0 && broken.Text == "" {
		t.Error("a multipart message without a boundary should not vanish")
	}
}

func TestSanitizeFilename(t *testing.T) {
	for input, want := range map[string]string{
		"../../etc/passwd": ".._.._etc_passwd",
		"":                 "attachment.bin",
		"normal.pdf":       "normal.pdf",
	} {
		if got := SanitizeFilename(input); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}
