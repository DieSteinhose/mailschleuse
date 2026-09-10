package smtpd

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/config"
	"github.com/diesteinhose/mailschleuse/internal/store"
	"github.com/diesteinhose/mailschleuse/internal/tlsutil"
)

// newTestServer starts a server on a random loopback port and returns its
// address plus the backing store.
func newTestServer(t *testing.T, mutate func(*Options)) (string, *store.Store) {
	t.Helper()
	messages, err := store.New(store.Options{
		Mailboxes:   []string{"inbox", "outbox"},
		MaxMessages: 50,
		MaxSize:     1 << 20,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	opts := Options{
		Hostname:       "mailschleuse.test",
		Auth:           config.AuthOptional,
		DefaultMailbox: "outbox",
		HeaderRouting:  true,
		AddReceived:    true,
		MaxSize:        1 << 20,
		MaxRecipients:  10,
		IdleTimeout:    5 * time.Second,
		Store:          messages,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(opts)
	go srv.Serve(ln, false)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), messages
}

func TestSendMailDeliversToDefaultMailbox(t *testing.T) {
	addr, messages := newTestServer(t, nil)

	body := "From: app@example.com\r\nTo: user@example.com\r\nSubject: Welcome\r\n\r\nHello there.\r\n"
	if err := smtp.SendMail(addr, nil, "app@example.com", []string{"user@example.com"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}

	list, total, _ := messages.List(store.ListOptions{Mailbox: "outbox"})
	if total != 1 {
		t.Fatalf("outbox total = %d, want 1", total)
	}
	if list[0].Subject != "Welcome" {
		t.Errorf("subject = %q", list[0].Subject)
	}
	if list[0].Envelope.From != "app@example.com" || list[0].Envelope.To[0] != "user@example.com" {
		t.Errorf("envelope = %+v", list[0].Envelope)
	}

	// Nothing may leak into the other mailbox: that separation is the point.
	if _, inboxTotal, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); inboxTotal != 0 {
		t.Errorf("inbox total = %d, want 0", inboxTotal)
	}
}

func TestReceivedHeaderIsPrepended(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	body := "Subject: Trace\r\n\r\nbody\r\n"
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}
	list, _, _ := messages.List(store.ListOptions{Mailbox: "outbox"})
	msg, _ := messages.Get(list[0].ID)
	if !strings.HasPrefix(string(msg.Raw()), "Received: from ") {
		t.Errorf("raw message does not start with a Received header:\n%s", msg.Raw())
	}
	if msg.Parsed().Subject != "Trace" {
		t.Errorf("subject after adding Received = %q", msg.Parsed().Subject)
	}
}

func TestRoutingHeaderSelectsMailbox(t *testing.T) {
	addr, messages := newTestServer(t, nil)

	body := fmt.Sprintf("%s: inbox\r\nSubject: Simulated customer\r\n\r\nhi\r\n", RoutingHeader)
	if err := smtp.SendMail(addr, nil, "customer@example.com", []string{"support@example.com"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 1 {
		t.Fatalf("inbox total = %d, want 1", total)
	}
}

func TestRoutingHeaderIgnoredWhenDisabled(t *testing.T) {
	addr, messages := newTestServer(t, func(o *Options) { o.HeaderRouting = false })

	body := fmt.Sprintf("%s: inbox\r\nSubject: x\r\n\r\nhi\r\n", RoutingHeader)
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "outbox"}); total != 1 {
		t.Errorf("outbox total = %d, want 1", total)
	}
}

func TestUsernameRoutingSelectsMailbox(t *testing.T) {
	addr, messages := newTestServer(t, func(o *Options) {
		o.UserRouting = true
		o.Username = ""
	})

	client, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Hello("test"); err != nil {
		t.Fatalf("ehlo: %v", err)
	}
	if err := client.Auth(smtp.PlainAuth("", "inbox", "anything", "127.0.0.1")); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := writeMessage(client, "customer@example.com", "support@example.com", "Subject: routed\r\n\r\nhi\r\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	client.Quit()

	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 1 {
		t.Errorf("inbox total = %d, want 1", total)
	}
}

func TestAuthRequiredRejectsAnonymous(t *testing.T) {
	addr, _ := newTestServer(t, func(o *Options) {
		o.Auth = config.AuthRequired
		o.Username = "dev"
		o.Password = "secret"
	})

	err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"}, []byte("Subject: x\r\n\r\nhi\r\n"))
	if err == nil {
		t.Fatal("anonymous delivery should be rejected when auth is required")
	}
	if !strings.Contains(err.Error(), "530") {
		t.Errorf("error = %v, want a 530 response", err)
	}
}

func TestAuthRequiredAcceptsValidCredentials(t *testing.T) {
	addr, messages := newTestServer(t, func(o *Options) {
		o.Auth = config.AuthRequired
		o.Username = "dev"
		o.Password = "secret"
	})

	auth := smtp.PlainAuth("", "dev", "secret", "127.0.0.1")
	if err := smtp.SendMail(addr, auth, "a@example.com", []string{"b@example.com"},
		[]byte("Subject: authed\r\n\r\nhi\r\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "outbox"}); total != 1 {
		t.Errorf("outbox total = %d, want 1", total)
	}
}

func TestWrongCredentialsRejected(t *testing.T) {
	addr, _ := newTestServer(t, func(o *Options) {
		o.Auth = config.AuthRequired
		o.Username = "dev"
		o.Password = "secret"
	})
	auth := smtp.PlainAuth("", "dev", "wrong", "127.0.0.1")
	err := smtp.SendMail(addr, auth, "a@example.com", []string{"b@example.com"}, []byte("Subject: x\r\n\r\nhi\r\n"))
	if err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("error = %v, want a 535 response", err)
	}
}

func TestDotStuffingIsUndone(t *testing.T) {
	addr, messages := newTestServer(t, nil)

	// A body line consisting of a single dot must survive the round trip.
	body := "Subject: dots\r\n\r\nline one\r\n.hidden\r\nline three\r\n"
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}
	list, _, _ := messages.List(store.ListOptions{Mailbox: "outbox"})
	msg, _ := messages.Get(list[0].ID)
	if !strings.Contains(msg.Parsed().Text, "\n.hidden") {
		t.Errorf("dot-stuffed line was mangled:\n%q", msg.Parsed().Text)
	}
}

func TestOversizedMessageRejected(t *testing.T) {
	addr, messages := newTestServer(t, func(o *Options) { o.MaxSize = 512 })

	body := "Subject: big\r\n\r\n" + strings.Repeat("x", 2048) + "\r\n"
	err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"}, []byte(body))
	if err == nil {
		t.Fatal("oversized message should be rejected")
	}
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "outbox"}); total != 0 {
		t.Errorf("oversized message was stored anyway")
	}

	// The session must still be usable for a message that fits.
	if err := smtp.SendMail(addr, nil, "a@example.com", []string{"b@example.com"},
		[]byte("Subject: small\r\n\r\nok\r\n")); err != nil {
		t.Fatalf("follow-up send: %v", err)
	}
}

func TestStartTLSUpgrade(t *testing.T) {
	cert, err := tlsutil.SelfSigned("mailschleuse.test")
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	addr, messages := newTestServer(t, func(o *Options) {
		o.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}}
	})

	client, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if err := client.Hello("test"); err != nil {
		t.Fatalf("ehlo: %v", err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS was not advertised")
	}
	if err := client.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatalf("starttls: %v", err)
	}
	if err := writeMessage(client, "a@example.com", "b@example.com", "Subject: encrypted\r\n\r\nhi\r\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	client.Quit()

	list, total, _ := messages.List(store.ListOptions{Mailbox: "outbox"})
	if total != 1 {
		t.Fatalf("outbox total = %d, want 1", total)
	}
	if !list[0].Envelope.TLS {
		t.Error("message is not flagged as received over TLS")
	}
}

func TestUnknownCommandDoesNotKillSession(t *testing.T) {
	addr, _ := newTestServer(t, nil)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, 512)
	conn.Read(buf) // greeting
	fmt.Fprint(conn, "FROBNICATE now\r\n")
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "502") {
		t.Errorf("response = %q, want a 502", buf[:n])
	}
	fmt.Fprint(conn, "QUIT\r\n")
}

// writeMessage runs the MAIL/RCPT/DATA sequence on an open client.
func writeMessage(client *smtp.Client, from, to, body string) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(body)); err != nil {
		return err
	}
	return w.Close()
}
