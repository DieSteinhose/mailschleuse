package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/mailparse"
	"github.com/diesteinhose/mailschleuse/internal/store"
)

func newTestServer(t *testing.T, mutate func(*Options)) (*httptest.Server, *store.Store) {
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
		Store:    messages,
		Version:  "test",
		Hostname: "mailschleuse.test",
		BasePath: "/",
		MaxSize:  1 << 20,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv := httptest.NewServer(New(opts))
	t.Cleanup(srv.Close)
	return srv, messages
}

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var payload map[string]any
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(raw, &payload)
	return resp, payload
}

func getJSON(t *testing.T, url string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var payload map[string]any
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(raw, &payload)
	return resp, payload
}

func TestComposeStoresMessage(t *testing.T) {
	srv, messages := newTestServer(t, nil)

	resp, payload := postJSON(t, srv.URL+"/api/messages", `{
		"mailbox": "inbox",
		"from": "Jane Customer <jane@example.com>",
		"to": ["support@example.com"],
		"subject": "Grüße",
		"text": "Mein Drucker ist kaputt.",
		"html": "<p>Mein Drucker ist kaputt.</p>"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, payload = %v", resp.StatusCode, payload)
	}

	msg, ok := messages.Get(payload["id"].(string))
	if !ok {
		t.Fatal("composed message was not stored")
	}
	parsed := msg.Parsed()
	if parsed.Subject != "Grüße" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	if parsed.From[0].Address != "jane@example.com" || parsed.From[0].Name != "Jane Customer" {
		t.Errorf("from = %+v", parsed.From)
	}
	if !strings.Contains(parsed.Text, "Drucker") || !strings.Contains(parsed.HTML, "<p>") {
		t.Errorf("bodies not preserved: text=%q html=%q", parsed.Text, parsed.HTML)
	}
	if msg.Mailbox != "inbox" {
		t.Errorf("mailbox = %q", msg.Mailbox)
	}
}

func TestComposeValidation(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	cases := map[string]string{
		"missing mailbox":   `{"from":"a@example.com","to":["b@example.com"],"text":"x"}`,
		"unknown mailbox":   `{"mailbox":"nope","from":"a@example.com","to":["b@example.com"],"text":"x"}`,
		"missing sender":    `{"mailbox":"inbox","to":["b@example.com"],"text":"x"}`,
		"missing recipient": `{"mailbox":"inbox","from":"a@example.com","text":"x"}`,
		"missing body":      `{"mailbox":"inbox","from":"a@example.com","to":["b@example.com"]}`,
	}
	for name, body := range cases {
		resp, payload := postJSON(t, srv.URL+"/api/messages", body)
		if resp.StatusCode < 400 {
			t.Errorf("%s: status = %d, want an error", name, resp.StatusCode)
		}
		if payload["error"] == nil {
			t.Errorf("%s: response carries no error message", name)
		}
	}
}

func TestComposeWithAttachment(t *testing.T) {
	srv, messages := newTestServer(t, nil)

	resp, payload := postJSON(t, srv.URL+"/api/messages", `{
		"mailbox": "inbox",
		"from": "a@example.com",
		"to": ["b@example.com"],
		"subject": "with file",
		"text": "see attachment",
		"attachments": [{"filename": "notes.txt", "contentType": "text/plain", "content": "aGVsbG8gd29ybGQ="}]
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, payload = %v", resp.StatusCode, payload)
	}

	msg, _ := messages.Get(payload["id"].(string))
	attachments := msg.Parsed().Attachments
	if len(attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(attachments))
	}
	if attachments[0].Filename != "notes.txt" || string(attachments[0].Content) != "hello world" {
		t.Errorf("attachment = %+v content=%q", attachments[0], attachments[0].Content)
	}

	// The attachment must be downloadable through the API.
	dl, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/attachments/" + attachments[0].PartID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer dl.Body.Close()
	got, _ := io.ReadAll(dl.Body)
	if string(got) != "hello world" {
		t.Errorf("downloaded %q", got)
	}
	if !strings.Contains(dl.Header.Get("Content-Disposition"), "notes.txt") {
		t.Errorf("content-disposition = %q", dl.Header.Get("Content-Disposition"))
	}
}

func TestHeaderInjectionIsBlocked(t *testing.T) {
	srv, messages := newTestServer(t, nil)

	resp, payload := postJSON(t, srv.URL+"/api/messages", `{
		"mailbox": "inbox",
		"from": "evil@example.com\r\nBcc: victim@example.com",
		"to": ["b@example.com"],
		"subject": "injected\r\nX-Evil: yes",
		"text": "body"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, payload = %v", resp.StatusCode, payload)
	}
	msg, _ := messages.Get(payload["id"].(string))
	for _, h := range msg.Parsed().Headers {
		if strings.EqualFold(h.Name, "Bcc") || strings.EqualFold(h.Name, "X-Evil") {
			t.Errorf("injected header %q survived", h.Name)
		}
	}
}

func TestListAndGetMessage(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	msg, _ := messages.Deliver("outbox", "smtp", store.Envelope{From: "app@example.com"},
		[]byte("Subject: hello\r\n\r\nbody\r\n"))

	resp, payload := getJSON(t, srv.URL+"/api/messages?mailbox=outbox")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if payload["total"].(float64) != 1 {
		t.Errorf("total = %v", payload["total"])
	}

	_, detail := getJSON(t, srv.URL+"/api/messages/"+msg.ID)
	inner := detail["message"].(map[string]any)
	if inner["subject"] != "hello" {
		t.Errorf("subject = %v", inner["subject"])
	}

	_, missing := getJSON(t, srv.URL+"/api/messages/does-not-exist")
	if missing["error"] == nil {
		t.Error("unknown message should return an error payload")
	}
}

func TestRawAndHTMLEndpoints(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	msg, _ := messages.Deliver("inbox", "web", store.Envelope{},
		[]byte("Subject: html\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>hi</p>\r\n"))

	raw, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/raw")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	defer raw.Body.Close()
	body, _ := io.ReadAll(raw.Body)
	if !strings.Contains(string(body), "Subject: html") {
		t.Errorf("raw body = %q", body)
	}

	preview, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/html")
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	defer preview.Body.Close()
	if csp := preview.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("preview is served without a restrictive CSP: %q", csp)
	}
	previewBody, _ := io.ReadAll(preview.Body)
	if !strings.Contains(string(previewBody), "<p>hi</p>") {
		t.Errorf("preview body = %q", previewBody)
	}
}

func TestReadFlagsAndDeletion(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	msg, _ := messages.Deliver("inbox", "web", store.Envelope{}, []byte("Subject: x\r\n\r\nbody\r\n"))

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/messages/"+msg.ID,
		strings.NewReader(`{"read":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("patch: %v status=%v", err, resp.Status)
	}
	resp.Body.Close()
	if stored, _ := messages.Get(msg.ID); !stored.Read() {
		t.Error("read flag was not set")
	}

	del, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/messages/"+msg.ID, nil)
	resp, err = http.DefaultClient.Do(del)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if _, ok := messages.Get(msg.ID); ok {
		t.Error("message still present after delete")
	}
}

func TestClearMailbox(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	messages.Deliver("inbox", "web", store.Envelope{}, []byte("Subject: x\r\n\r\nbody\r\n"))

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/mailboxes/inbox/messages", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	defer resp.Body.Close()
	if _, total, _ := messages.List(store.ListOptions{Mailbox: "inbox"}); total != 0 {
		t.Errorf("mailbox not empty after clear")
	}
}

func TestImportRawMessage(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	resp, err := http.Post(srv.URL+"/api/messages/import?mailbox=inbox", "message/rfc822",
		strings.NewReader("Subject: imported\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	list, _, _ := messages.List(store.ListOptions{Mailbox: "inbox"})
	if len(list) != 1 || list[0].Subject != "imported" {
		t.Errorf("imported message = %+v", list)
	}
}

func TestReadOnlyModeBlocksWrites(t *testing.T) {
	srv, _ := newTestServer(t, func(o *Options) { o.ReadOnly = true })
	resp, _ := postJSON(t, srv.URL+"/api/messages", `{"mailbox":"inbox","from":"a@b.c","to":["d@e.f"],"text":"x"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestBasicAuthProtectsAPI(t *testing.T) {
	srv, _ := newTestServer(t, func(o *Options) {
		o.Username = "admin"
		o.Password = "hunter2"
	})

	resp, err := http.Get(srv.URL + "/api/mailboxes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/mailboxes", nil)
	req.SetBasicAuth("admin", "hunter2")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated status = %d", resp.StatusCode)
	}

	// Health checks stay reachable so container probes keep working.
	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", health.StatusCode)
	}
}

func TestUIIsServed(t *testing.T) {
	srv, _ := newTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Mailschleuse") {
		t.Errorf("index page does not look like the UI")
	}

	// A deep link must fall back to the app shell instead of 404ing.
	deep, err := http.Get(srv.URL + "/some/unknown/path")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer deep.Body.Close()
	if deep.StatusCode != http.StatusOK {
		t.Errorf("deep link status = %d", deep.StatusCode)
	}
}

func TestBasePathPrefix(t *testing.T) {
	srv, _ := newTestServer(t, func(o *Options) { o.BasePath = "/mail" })

	resp, err := http.Get(srv.URL + "/mail/api/mailboxes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("prefixed status = %d", resp.StatusCode)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	bare, err := client.Get(srv.URL + "/api/mailboxes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	bare.Body.Close()
	if bare.StatusCode == http.StatusOK {
		t.Error("API is reachable outside the configured base path")
	}
}

func TestEventStreamDeliversNewMessages(t *testing.T) {
	srv, messages := newTestServer(t, nil)

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	messages.Deliver("inbox", "web", store.Envelope{}, []byte("Subject: streamed\r\n\r\nbody\r\n"))

	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	chunk := string(buf[:n])
	for !strings.Contains(chunk, "message.new") {
		n, err = resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("stream ended without the event: %q", chunk)
		}
		chunk += string(buf[:n])
	}
	if !strings.Contains(chunk, "streamed") {
		t.Errorf("event payload lacks the message summary: %q", chunk)
	}
}

func TestComposeBuildProducesParseableMessage(t *testing.T) {
	req := ComposeRequest{
		From:    "Ärger Müller <mueller@example.com>",
		To:      []string{"support@example.com", "second@example.com"},
		Cc:      []string{"cc@example.com"},
		Subject: "Änderung",
		Text:    "Mit Ümlauten\nund zwei Zeilen",
		Headers: map[string]string{"X-Ticket": "42", "Subject": "should be ignored"},
	}
	raw, err := req.Build("mailschleuse.test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(string(raw), "\r\n\r\n") {
		t.Fatal("built message has no header/body separator")
	}

	msg := mailparse.Parse(raw)
	if msg.Subject != "Änderung" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if len(msg.To) != 2 {
		t.Errorf("recipients = %+v", msg.To)
	}
	if msg.From[0].Name != "Ärger Müller" {
		t.Errorf("from name = %q", msg.From[0].Name)
	}
	if !strings.Contains(msg.Text, "Mit Ümlauten") {
		t.Errorf("text = %q", msg.Text)
	}

	var ticket, subjectCount int
	for _, h := range msg.Headers {
		if h.Name == "X-Ticket" {
			ticket++
		}
		if strings.EqualFold(h.Name, "Subject") {
			subjectCount++
		}
	}
	if ticket != 1 {
		t.Errorf("custom header count = %d, want 1", ticket)
	}
	if subjectCount != 1 {
		t.Errorf("subject header count = %d; the headers map must not duplicate it", subjectCount)
	}
}

func TestAttachmentsAreNeverRenderedInline(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	// An inline HTML part is attacker-controlled content on our own origin.
	msg, _ := messages.Deliver("inbox", "smtp", store.Envelope{}, []byte(
		"Subject: hostile\r\n"+
			"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n"+
			"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n"+
			"--B\r\nContent-Type: text/html\r\nContent-Disposition: inline; filename=\"x.html\"\r\n\r\n"+
			"<script>alert(1)</script>\r\n--B--\r\n"))

	var part string
	for _, a := range msg.Parsed().Attachments {
		if strings.HasPrefix(a.ContentType, "text/html") {
			part = a.PartID
		}
	}
	if part == "" {
		t.Fatal("test message does not contain the HTML part")
	}

	resp, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/attachments/" + part)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if disposition := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("content-disposition = %q, want an attachment", disposition)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("attachment served without a sandbox CSP: %q", csp)
	}
}

func TestInlineImageIsRendered(t *testing.T) {
	srv, messages := newTestServer(t, nil)
	msg, _ := messages.Deliver("inbox", "smtp", store.Envelope{}, []byte(
		"Subject: logo\r\n"+
			"Content-Type: multipart/related; boundary=\"B\"\r\n\r\n"+
			"--B\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:logo\">\r\n"+
			"--B\r\nContent-Type: image/png\r\nContent-ID: <logo>\r\n"+
			"Content-Disposition: inline; filename=\"logo.png\"\r\n"+
			"Content-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n--B--\r\n"))

	var part string
	for _, a := range msg.Parsed().Attachments {
		if a.ContentType == "image/png" {
			part = a.PartID
		}
	}
	if part == "" {
		t.Fatal("inline image part not found")
	}

	resp, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/attachments/" + part)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if disposition := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disposition, "inline") {
		t.Errorf("content-disposition = %q, want inline for an image", disposition)
	}

	// The preview must point the cid: reference at that endpoint.
	preview, err := http.Get(srv.URL + "/api/messages/" + msg.ID + "/html")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	defer preview.Body.Close()
	body, _ := io.ReadAll(preview.Body)
	if strings.Contains(string(body), "cid:logo") {
		t.Errorf("cid reference was not rewritten: %q", body)
	}
	if !strings.Contains(string(body), "attachments/"+part) {
		t.Errorf("preview does not reference the attachment endpoint: %q", body)
	}
}

// TestShutdownReleasesEventStream guards the reason container stops used to
// take the full grace period and end in SIGKILL: the event stream is never
// idle, so a graceful shutdown waited for it until the runtime gave up.
func TestShutdownReleasesEventStream(t *testing.T) {
	messages, err := store.New(store.Options{Mailboxes: []string{"inbox"}, MaxMessages: 10, MaxSize: 1 << 20})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	handler := New(Options{
		Store:    messages,
		BasePath: "/",
		MaxSize:  1 << 20,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	httpServer := &http.Server{Handler: handler}
	httpServer.RegisterOnShutdown(handler.Shutdown)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go httpServer.Serve(listener)

	resp, err := http.Get("http://" + listener.Addr().String() + "/api/events")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()

	// Read the preamble so the request is definitely in flight.
	buf := make([]byte, 256)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read preamble: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- httpServer.Shutdown(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown did not complete: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown is still waiting for the event stream")
	}

	// The stream itself must have ended rather than been cut mid-flight.
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(buf)+string(rest), "shutdown") {
		t.Errorf("client was not told about the shutdown: %q", string(buf)+string(rest))
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	handler := New(Options{BasePath: "/", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	handler.Shutdown()
	handler.Shutdown() // must not panic on a second close
}
