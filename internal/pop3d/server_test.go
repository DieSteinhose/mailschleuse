package pop3d

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/store"
)

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
		DefaultMailbox: "inbox",
		ExclusiveLock:  true,
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

// client is a minimal POP3 test client.
type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	t.Cleanup(func() { conn.Close() })
	if greeting := c.readLine(); !strings.HasPrefix(greeting, "+OK") {
		t.Fatalf("greeting = %q", greeting)
	}
	return c
}

func (c *client) readLine() string {
	c.t.Helper()
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// cmd sends a command and returns the single status line.
func (c *client) cmd(format string) string {
	c.t.Helper()
	if _, err := io.WriteString(c.conn, format+"\r\n"); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	return c.readLine()
}

// multi sends a command and returns the status line plus the dot-terminated body.
func (c *client) multi(format string) (string, []string) {
	c.t.Helper()
	status := c.cmd(format)
	if !strings.HasPrefix(status, "+OK") {
		return status, nil
	}
	var lines []string
	for {
		line := c.readLine()
		if line == "." {
			return status, lines
		}
		lines = append(lines, strings.TrimPrefix(line, "."))
	}
}

func (c *client) login(user, pass string) {
	c.t.Helper()
	if resp := c.cmd("USER " + user); !strings.HasPrefix(resp, "+OK") {
		c.t.Fatalf("USER = %q", resp)
	}
	if resp := c.cmd("PASS " + pass); !strings.HasPrefix(resp, "+OK") {
		c.t.Fatalf("PASS = %q", resp)
	}
}

func seed(t *testing.T, messages *store.Store, mailbox string, subjects ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		msg, err := messages.Deliver(mailbox, "web", store.Envelope{From: "customer@example.com"},
			[]byte("From: customer@example.com\r\nSubject: "+subject+"\r\n\r\nbody of "+subject+"\r\n"))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		ids = append(ids, msg.ID)
	}
	return ids
}

func TestStatListAndRetr(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	ids := seed(t, messages, "inbox", "one", "two")

	c := dial(t, addr)
	c.login("inbox", "whatever")

	stat := c.cmd("STAT")
	if !strings.HasPrefix(stat, "+OK 2 ") {
		t.Errorf("STAT = %q, want 2 messages", stat)
	}

	_, list := c.multi("LIST")
	if len(list) != 2 || !strings.HasPrefix(list[0], "1 ") {
		t.Errorf("LIST = %v", list)
	}

	_, uidl := c.multi("UIDL")
	if len(uidl) != 2 || !strings.HasSuffix(uidl[0], ids[0]) {
		t.Errorf("UIDL = %v, want stable ids %v", uidl, ids)
	}

	_, body := c.multi("RETR 1")
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, "Subject: one") || !strings.Contains(joined, "body of one") {
		t.Errorf("RETR body = %q", joined)
	}
}

func TestPop3OnlyServesItsOwnMailbox(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	seed(t, messages, "outbox", "outgoing notification")

	c := dial(t, addr)
	c.login("inbox", "x")
	if stat := c.cmd("STAT"); !strings.HasPrefix(stat, "+OK 0 0") {
		t.Fatalf("STAT = %q; outbox mail must not be fetchable from the inbox endpoint", stat)
	}
}

func TestDeleteAppliesOnQuit(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	seed(t, messages, "inbox", "one", "two")

	c := dial(t, addr)
	c.login("inbox", "x")
	if resp := c.cmd("DELE 1"); !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("DELE = %q", resp)
	}
	// Before QUIT the message must still exist in the store.
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 2 {
		t.Fatalf("store total = %d before QUIT, want 2", total)
	}
	if resp := c.cmd("RETR 1"); !strings.HasPrefix(resp, "-ERR") {
		t.Errorf("RETR of a deleted message = %q, want -ERR", resp)
	}
	if resp := c.cmd("QUIT"); !strings.HasPrefix(resp, "+OK") {
		t.Fatalf("QUIT = %q", resp)
	}

	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 1 {
		t.Errorf("store total = %d after QUIT, want 1", total)
	}
}

func TestResetUndoesDeletion(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	seed(t, messages, "inbox", "one")

	c := dial(t, addr)
	c.login("inbox", "x")
	c.cmd("DELE 1")
	c.cmd("RSET")
	c.cmd("QUIT")

	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 1 {
		t.Errorf("RSET did not undo the deletion")
	}
}

func TestSessionSnapshotIsFrozen(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	seed(t, messages, "inbox", "one")

	c := dial(t, addr)
	c.login("inbox", "x")
	seed(t, messages, "inbox", "arrived later")

	if stat := c.cmd("STAT"); !strings.HasPrefix(stat, "+OK 1 ") {
		t.Errorf("STAT = %q; a message that arrived mid-session must not appear", stat)
	}
}

func TestTopReturnsHeadersAndLimitedBody(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	messages.Deliver("inbox", "web", store.Envelope{},
		[]byte("Subject: top\r\n\r\nline1\r\nline2\r\nline3\r\n"))

	c := dial(t, addr)
	c.login("inbox", "x")
	_, body := c.multi("TOP 1 1")
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, "Subject: top") {
		t.Errorf("TOP is missing the headers: %q", joined)
	}
	if !strings.Contains(joined, "line1") || strings.Contains(joined, "line2") {
		t.Errorf("TOP body = %q, want exactly one body line", joined)
	}
}

func TestDotStuffingOnRetr(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	messages.Deliver("inbox", "web", store.Envelope{},
		[]byte("Subject: dots\r\n\r\n.leading dot\r\nnormal\r\n"))

	c := dial(t, addr)
	c.login("inbox", "x")
	status := c.cmd("RETR 1")
	if !strings.HasPrefix(status, "+OK") {
		t.Fatalf("RETR = %q", status)
	}
	var raw []string
	for {
		line := c.readLine()
		if line == "." {
			break
		}
		raw = append(raw, line)
	}
	if !containsLine(raw, "..leading dot") {
		t.Errorf("leading dot was not stuffed on the wire: %v", raw)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestOctetCountMatchesTransferredBytes(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	// A message stored with bare LF endings must still be counted as CRLF.
	messages.Deliver("inbox", "web", store.Envelope{}, []byte("Subject: lf\n\n.dot\nbody\n"))

	c := dial(t, addr)
	c.login("inbox", "x")
	_, list := c.multi("LIST")
	if len(list) != 1 {
		t.Fatalf("LIST = %v", list)
	}
	announced := strings.Fields(list[0])[1]

	status := c.cmd("RETR 1")
	if !strings.HasPrefix(status, "+OK") {
		t.Fatalf("RETR = %q", status)
	}
	transferred := 0
	for {
		line := c.readLine()
		if line == "." {
			break
		}
		transferred += len(line) + 2
	}
	if announced != itoa(transferred) {
		t.Errorf("LIST announced %s octets, transferred %d", announced, transferred)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func TestExclusiveLock(t *testing.T) {
	addr, messages := newTestServer(t, nil)
	seed(t, messages, "inbox", "one")

	first := dial(t, addr)
	first.login("inbox", "x")

	second := dial(t, addr)
	second.cmd("USER inbox")
	if resp := second.cmd("PASS x"); !strings.Contains(resp, "IN-USE") {
		t.Errorf("second session PASS = %q, want an IN-USE error", resp)
	}

	first.cmd("QUIT")
	// After the first session ends the mailbox must be available again.
	third := dial(t, addr)
	third.login("inbox", "x")
}

func TestCredentialsEnforced(t *testing.T) {
	addr, _ := newTestServer(t, func(o *Options) {
		o.Username = "mailclient"
		o.Password = "secret"
	})

	c := dial(t, addr)
	c.cmd("USER mailclient")
	if resp := c.cmd("PASS wrong"); !strings.HasPrefix(resp, "-ERR") {
		t.Errorf("wrong password accepted: %q", resp)
	}
	if resp := c.cmd("PASS secret"); !strings.HasPrefix(resp, "-ERR") {
		t.Errorf("PASS without a preceding USER should fail: %q", resp)
	}
	c.cmd("USER mailclient")
	if resp := c.cmd("PASS secret"); !strings.HasPrefix(resp, "+OK") {
		t.Errorf("valid credentials rejected: %q", resp)
	}
}

func TestUserRoutingSelectsMailbox(t *testing.T) {
	addr, messages := newTestServer(t, func(o *Options) { o.UserRouting = true })
	seed(t, messages, "outbox", "notification")

	c := dial(t, addr)
	c.login("outbox", "x")
	if stat := c.cmd("STAT"); !strings.HasPrefix(stat, "+OK 1 ") {
		t.Errorf("STAT = %q, want the outbox contents", stat)
	}
}

func TestCapaAdvertisesUidlAndTop(t *testing.T) {
	addr, _ := newTestServer(t, nil)
	c := dial(t, addr)
	_, caps := c.multi("CAPA")
	joined := strings.Join(caps, " ")
	for _, want := range []string{"UIDL", "TOP", "USER"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CAPA is missing %s: %v", want, caps)
		}
	}
}

func TestCommandsRequireAuthentication(t *testing.T) {
	addr, _ := newTestServer(t, nil)
	c := dial(t, addr)
	for _, cmd := range []string{"STAT", "LIST", "RETR 1", "DELE 1", "UIDL"} {
		if resp := c.cmd(cmd); !strings.HasPrefix(resp, "-ERR") {
			t.Errorf("%s before authentication = %q, want -ERR", cmd, resp)
		}
	}
}
