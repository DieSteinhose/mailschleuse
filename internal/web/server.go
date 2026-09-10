// Package web serves the single-page user interface and the REST API. Both
// mailboxes live behind one origin so switching between "what my application
// sent" and "what my application will receive" is a click, not a second tool.
package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/mailparse"
	"github.com/diesteinhose/mailschleuse/internal/store"
)

// Endpoint describes a protocol listener for the UI's connection panel.
type Endpoint struct {
	Protocol     string `json:"protocol"`
	Host         string `json:"host"`
	Port         string `json:"port"`
	Mailbox      string `json:"mailbox"`
	Username     string `json:"username"`
	AuthRequired bool   `json:"authRequired"`
	TLS          string `json:"tls"`
	Note         string `json:"note,omitempty"`
}

// Options configures the HTTP server.
type Options struct {
	Store      *store.Store
	Version    string
	Hostname   string
	BasePath   string
	Username   string
	Password   string
	CORSOrigin string
	ReadOnly   bool
	MaxSize    int64
	Endpoints  []Endpoint
	Logger     *slog.Logger
}

// Server is the HTTP handler for API and UI.
type Server struct {
	opts Options
}

// NewHandler builds the HTTP handler tree.
func NewHandler(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/mailboxes", s.handleMailboxes)
	mux.HandleFunc("DELETE /api/mailboxes/{name}/messages", s.handleClearMailbox)
	mux.HandleFunc("POST /api/mailboxes/{name}/read", s.handleMarkAllRead)
	mux.HandleFunc("GET /api/messages", s.handleListMessages)
	mux.HandleFunc("POST /api/messages", s.handleCompose)
	mux.HandleFunc("POST /api/messages/import", s.handleImport)
	mux.HandleFunc("GET /api/messages/{id}", s.handleGetMessage)
	mux.HandleFunc("PATCH /api/messages/{id}", s.handlePatchMessage)
	mux.HandleFunc("DELETE /api/messages/{id}", s.handleDeleteMessage)
	mux.HandleFunc("GET /api/messages/{id}/raw", s.handleRawMessage)
	mux.HandleFunc("GET /api/messages/{id}/html", s.handleHTMLMessage)
	mux.HandleFunc("GET /api/messages/{id}/attachments/{part}", s.handleAttachment)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.Handle("GET /", s.staticHandler())

	var handler http.Handler = mux
	handler = s.withReadOnly(handler)
	handler = s.withAuth(handler)
	handler = s.withCORS(handler)
	handler = s.withLogging(handler)

	if base := strings.TrimSuffix(opts.BasePath, "/"); base != "" {
		outer := http.NewServeMux()
		outer.Handle(base+"/", http.StripPrefix(base, handler))
		outer.Handle(base, http.RedirectHandler(base+"/", http.StatusMovedPermanently))
		return outer
	}
	return handler
}

// --- middleware -------------------------------------------------------------

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 {
			s.opts.Logger.Warn("http request failed",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration", time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps the recorder transparent for the SSE handler.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.opts.Username == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The health endpoint stays open so container probes work unauthenticated.
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.opts.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.opts.Password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Mailschleuse"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	if s.opts.CORSOrigin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.opts.CORSOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withReadOnly turns the instance into a viewer that cannot be modified through
// the API, which is useful when the UI is shared with a wider audience.
func (s *Server) withReadOnly(next http.Handler) http.Handler {
	if !s.opts.ReadOnly {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodPut:
			writeError(w, http.StatusForbidden, "instance runs in read-only mode")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.opts.Version})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.opts.Version,
		"hostname":       s.opts.Hostname,
		"mailboxes":      s.opts.Store.Stats(),
		"endpoints":      s.opts.Endpoints,
		"readOnly":       s.opts.ReadOnly,
		"maxMessageSize": s.opts.MaxSize,
	})
}

func (s *Server) handleMailboxes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": s.opts.Store.Stats()})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	mailbox := query.Get("mailbox")
	if mailbox != "" && !s.opts.Store.Has(mailbox) {
		writeError(w, http.StatusNotFound, "no such mailbox: %s", mailbox)
		return
	}
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	messages, total, err := s.opts.Store.List(store.ListOptions{
		Mailbox: mailbox,
		Search:  query.Get("search"),
		Limit:   limit,
		Offset:  offset,
		Unread:  query.Get("unread") == "true",
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"total":    total,
		"offset":   offset,
	})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.opts.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         msg.ID,
		"mailbox":    msg.Mailbox,
		"receivedAt": msg.ReceivedAt,
		"source":     msg.Source,
		"envelope":   msg.Envelope,
		"size":       msg.Size,
		"read":       msg.Read(),
		"message":    msg.Parsed(),
	})
}

func (s *Server) handleRawMessage(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.opts.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822; charset=utf-8")
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", msg.ID+".eml"))
	}
	w.Write(msg.Raw())
}

// handleHTMLMessage serves the HTML body for the preview iframe. Remote content
// is blocked by policy so opening a stored mail cannot phone home, and inline
// images referenced by cid: are rewritten to the attachment endpoint.
func (s *Server) handleHTMLMessage(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.opts.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	html := msg.Parsed().HTML
	if html == "" {
		html = "<!doctype html><meta charset=\"utf-8\"><p style=\"font:14px system-ui;color:#666\">This message has no HTML part.</p>"
	}
	html = rewriteCID(html, msg)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; font-src data:; base-uri 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	io.WriteString(w, html)
}

// rewriteCID points cid: references at the attachment endpoint so inline images
// render in the preview.
func rewriteCID(html string, msg *store.Message) string {
	replacements := make([]string, 0, len(msg.Parsed().Attachments)*4)
	for _, a := range msg.Parsed().Attachments {
		if a.ContentID == "" {
			continue
		}
		target := fmt.Sprintf("../%s/attachments/%s", msg.ID, a.PartID)
		replacements = append(replacements, "cid:"+a.ContentID, target)
		replacements = append(replacements, "CID:"+a.ContentID, target)
	}
	if len(replacements) == 0 {
		return html
	}
	return strings.NewReplacer(replacements...).Replace(html)
}

func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.opts.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	part := r.PathValue("part")
	for _, a := range msg.Parsed().Attachments {
		if a.PartID != part {
			continue
		}
		w.Header().Set("Content-Type", a.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// An attachment is attacker-supplied content served from the UI's own
		// origin, so only images ever render in place; everything else is
		// handed to the browser as a download and sandboxed on top of that.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; sandbox")
		disposition := "attachment"
		if a.Inline && renderableInline(a.ContentType) && r.URL.Query().Get("download") != "true" {
			disposition = "inline"
		}
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("%s; filename=%q", disposition, mailparse.SanitizeFilename(a.Filename)))
		w.Write(a.Content)
		return
	}
	writeError(w, http.StatusNotFound, "no such attachment: %s", part)
}

// renderableInline reports whether a part may be displayed rather than
// downloaded. Only image types qualify: they are what inline mail parts use,
// and they cannot execute script.
func renderableInline(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(base)), "image/")
}

func (s *Server) handlePatchMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Read *bool `json:"read"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if body.Read == nil {
		writeError(w, http.StatusBadRequest, "nothing to update: set \"read\"")
		return
	}
	if !s.opts.Store.MarkRead(r.PathValue("id"), *body.Read) {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Store.Delete(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleClearMailbox(w http.ResponseWriter, r *http.Request) {
	removed, err := s.opts.Store.Clear(r.PathValue("name"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": removed})
}

func (s *Server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	updated, err := s.opts.Store.MarkAllRead(r.PathValue("name"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// handleCompose builds a message from structured fields and drops it into a
// mailbox, which is how a developer simulates incoming mail.
func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	var req ComposeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	mailbox := strings.TrimSpace(req.Mailbox)
	if mailbox == "" {
		writeError(w, http.StatusBadRequest, "field \"mailbox\" is required")
		return
	}
	if !s.opts.Store.Has(mailbox) {
		writeError(w, http.StatusNotFound, "no such mailbox: %s", mailbox)
		return
	}

	raw, err := req.Build(s.opts.Hostname)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	recipients := cleanList(req.To)
	msg, err := s.opts.Store.Deliver(mailbox, "web", store.Envelope{
		From:       strings.TrimSpace(req.From),
		To:         append(recipients, append(cleanList(req.Cc), cleanList(req.Bcc)...)...),
		RemoteAddr: r.RemoteAddr,
	}, raw)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.opts.Logger.Info("message accepted",
		"protocol", "http", "mailbox", mailbox, "id", msg.ID, "from", msg.Envelope.From, "bytes", msg.Size)
	writeJSON(w, http.StatusCreated, map[string]any{"id": msg.ID, "mailbox": mailbox})
}

// handleImport stores a complete .eml file, which makes it easy to replay a
// message captured somewhere else.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	mailbox := strings.TrimSpace(r.URL.Query().Get("mailbox"))
	if mailbox == "" || !s.opts.Store.Has(mailbox) {
		writeError(w, http.StatusNotFound, "no such mailbox: %s", mailbox)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxSize))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "message too large")
		return
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "empty request body")
		return
	}
	msg, err := s.opts.Store.Deliver(mailbox, "import", store.Envelope{RemoteAddr: r.RemoteAddr}, raw)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": msg.ID, "mailbox": mailbox})
}

// handleEvents streams store changes to the UI so both mailboxes update live.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	events, unsubscribe := s.opts.Store.Subscribe()
	defer unsubscribe()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
			flusher.Flush()
		}
	}
}

// --- helpers ----------------------------------------------------------------

func decodeJSON(r *http.Request, into any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNoSuchMailbox):
		writeError(w, http.StatusNotFound, "%s", err)
	case errors.Is(err, store.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "%s", err)
	default:
		writeError(w, http.StatusInternalServerError, "%s", err)
	}
}
