// Package smtpd implements the SMTP submission endpoint. It speaks enough
// ESMTP for real mail libraries - EHLO, PIPELINING, SIZE, 8BITMIME, STARTTLS
// and AUTH PLAIN/LOGIN - and delivers everything it accepts into a mailbox
// instead of relaying it anywhere.
package smtpd

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/config"
	"github.com/diesteinhose/mailschleuse/internal/store"
)

// RoutingHeader lets a client pick the target mailbox per message.
const RoutingHeader = "X-Mailschleuse-Mailbox"

// maxLineBytes caps a single protocol or data line.
const maxLineBytes = 1 << 20

// Options configures the SMTP server.
type Options struct {
	Hostname       string
	Auth           config.AuthMode
	Username       string
	Password       string
	DefaultMailbox string
	UserRouting    bool
	HeaderRouting  bool
	AddReceived    bool
	MaxSize        int64
	MaxRecipients  int
	IdleTimeout    time.Duration
	TLSConfig      *tls.Config
	Store          *store.Store
	Logger         *slog.Logger
}

// Server accepts SMTP connections until it is closed.
type Server struct {
	opts Options

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	closed    bool
	wg        sync.WaitGroup
}

// New creates a server from the given options.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxRecipients <= 0 {
		opts.MaxRecipients = 100
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Server{opts: opts, conns: make(map[net.Conn]struct{})}
}

// Serve accepts connections on ln until the server is closed. When implicitTLS
// is true the connection is wrapped in TLS before the greeting is sent.
func (s *Server) Serve(ln net.Listener, implicitTLS bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if implicitTLS {
			conn = tls.Server(conn, s.opts.TLSConfig)
		}
		s.track(conn, true)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.track(conn, false)
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

// Close stops all listeners and open connections and waits for the handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, ln := range s.listeners {
		ln.Close()
	}
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(conn net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[conn] = struct{}{}
		return
	}
	delete(s.conns, conn)
}

// session is the per-connection protocol state.
type session struct {
	srv  *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	helo          string
	tlsActive     bool
	authenticated bool
	username      string

	from   string
	rcpts  []string
	inMail bool
}

func (s *Server) handle(conn net.Conn) {
	sess := &session{
		srv:  s,
		conn: conn,
		r:    bufio.NewReaderSize(conn, 4096),
		w:    bufio.NewWriterSize(conn, 4096),
	}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		sess.tlsActive = true
	}
	sess.run()
}

func (s *session) run() {
	s.reply(220, "%s Mailschleuse ESMTP ready", s.srv.opts.Hostname)
	for {
		line, err := s.readLine()
		if err != nil {
			return
		}
		verb, args := splitCommand(line)
		switch verb {
		case "EHLO":
			s.handleEhlo(args, true)
		case "HELO":
			s.handleEhlo(args, false)
		case "STARTTLS":
			if !s.handleStartTLS() {
				return
			}
		case "AUTH":
			s.handleAuth(args)
		case "MAIL":
			s.handleMail(args)
		case "RCPT":
			s.handleRcpt(args)
		case "DATA":
			s.handleData()
		case "RSET":
			s.resetTransaction()
			s.reply(250, "2.0.0 OK")
		case "NOOP":
			s.reply(250, "2.0.0 OK")
		case "VRFY", "EXPN":
			s.reply(252, "2.5.2 Cannot verify, but will accept the message")
		case "HELP":
			s.reply(214, "2.0.0 Mailschleuse - every accepted message is stored, never relayed")
		case "QUIT":
			s.reply(221, "2.0.0 %s closing connection", s.srv.opts.Hostname)
			return
		case "":
			s.reply(500, "5.5.2 Error: bad syntax")
		default:
			s.reply(502, "5.5.1 Error: command %q not implemented", verb)
		}
	}
}

func (s *session) handleEhlo(args string, extended bool) {
	s.helo = strings.TrimSpace(args)
	if s.helo == "" {
		s.reply(501, "5.5.4 Syntax: EHLO hostname")
		return
	}
	s.resetTransaction()
	if !extended {
		s.reply(250, "%s", s.srv.opts.Hostname)
		return
	}

	lines := []string{
		fmt.Sprintf("%s greets %s", s.srv.opts.Hostname, s.helo),
		fmt.Sprintf("SIZE %d", s.srv.opts.MaxSize),
		"8BITMIME",
		"SMTPUTF8",
		"PIPELINING",
		"ENHANCEDSTATUSCODES",
	}
	if s.srv.opts.TLSConfig != nil && !s.tlsActive {
		lines = append(lines, "STARTTLS")
	}
	if s.srv.opts.Auth != config.AuthDisabled {
		lines = append(lines, "AUTH PLAIN LOGIN")
	}
	s.multiReply(250, lines)
}

func (s *session) handleStartTLS() bool {
	if s.srv.opts.TLSConfig == nil {
		s.reply(454, "4.7.0 TLS not available")
		return true
	}
	if s.tlsActive {
		s.reply(503, "5.5.1 TLS already active")
		return true
	}
	s.reply(220, "2.0.0 Ready to start TLS")
	tlsConn := tls.Server(s.conn, s.srv.opts.TLSConfig)
	s.conn.SetDeadline(time.Now().Add(s.srv.opts.IdleTimeout))
	if err := tlsConn.Handshake(); err != nil {
		s.srv.opts.Logger.Debug("smtp starttls handshake failed", "error", err)
		return false
	}
	// RFC 3207: discard all state negotiated before the handshake.
	s.conn = tlsConn
	s.r = bufio.NewReaderSize(tlsConn, 4096)
	s.w = bufio.NewWriterSize(tlsConn, 4096)
	s.tlsActive = true
	s.helo = ""
	s.authenticated = false
	s.username = ""
	s.resetTransaction()
	return true
}

func (s *session) handleAuth(args string) {
	if s.srv.opts.Auth == config.AuthDisabled {
		s.reply(502, "5.5.1 AUTH not available")
		return
	}
	if s.authenticated {
		s.reply(503, "5.5.1 Already authenticated")
		return
	}
	mechanism, rest := splitCommand(args)
	switch strings.ToUpper(mechanism) {
	case "PLAIN":
		token := strings.TrimSpace(rest)
		if token == "" {
			s.reply(334, "")
			line, err := s.readLine()
			if err != nil {
				return
			}
			token = strings.TrimSpace(line)
		}
		decoded, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			s.reply(501, "5.5.2 Cannot decode AUTH PLAIN response")
			return
		}
		parts := strings.Split(string(decoded), "\x00")
		if len(parts) != 3 {
			s.reply(501, "5.5.2 Malformed AUTH PLAIN response")
			return
		}
		s.finishAuth(parts[1], parts[2])
	case "LOGIN":
		username := strings.TrimSpace(rest)
		if username == "" {
			s.reply(334, "%s", base64.StdEncoding.EncodeToString([]byte("Username:")))
			line, err := s.readLine()
			if err != nil {
				return
			}
			username = strings.TrimSpace(line)
		}
		s.reply(334, "%s", base64.StdEncoding.EncodeToString([]byte("Password:")))
		passLine, err := s.readLine()
		if err != nil {
			return
		}
		decodedUser, err1 := base64.StdEncoding.DecodeString(username)
		decodedPass, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(passLine))
		if err1 != nil || err2 != nil {
			s.reply(501, "5.5.2 Cannot decode AUTH LOGIN response")
			return
		}
		s.finishAuth(string(decodedUser), string(decodedPass))
	default:
		s.reply(504, "5.5.4 Unsupported authentication mechanism")
	}
}

func (s *session) finishAuth(username, password string) {
	if !s.srv.credentialsValid(username, password) {
		s.srv.opts.Logger.Info("smtp auth rejected", "user", username, "remote", s.remoteAddr())
		s.reply(535, "5.7.8 Authentication credentials invalid")
		return
	}
	s.authenticated = true
	s.username = username
	s.reply(235, "2.7.0 Authentication successful")
}

// credentialsValid checks a login. With no credentials configured any login is
// accepted, which keeps throwaway development setups working without a .env.
func (s *Server) credentialsValid(username, password string) bool {
	if s.opts.Username == "" {
		return true
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.opts.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.opts.Password)) == 1
	return userOK && passOK
}

func (s *session) handleMail(args string) {
	if s.helo == "" {
		s.reply(503, "5.5.1 Send HELO/EHLO first")
		return
	}
	if s.srv.opts.Auth == config.AuthRequired && !s.authenticated {
		s.reply(530, "5.7.0 Authentication required")
		return
	}
	if s.inMail {
		s.reply(503, "5.5.1 Nested MAIL command")
		return
	}
	rest, ok := cutPrefixFold(args, "FROM:")
	if !ok {
		s.reply(501, "5.5.4 Syntax: MAIL FROM:<address>")
		return
	}
	addr, params, err := parsePath(rest)
	if err != nil {
		s.reply(501, "5.5.4 %s", err)
		return
	}
	if sizeParam, ok := params["SIZE"]; ok {
		var declared int64
		if _, err := fmt.Sscanf(sizeParam, "%d", &declared); err == nil && declared > s.srv.opts.MaxSize {
			s.reply(552, "5.3.4 Message size %d exceeds limit of %d bytes", declared, s.srv.opts.MaxSize)
			return
		}
	}
	s.from = addr
	s.inMail = true
	s.reply(250, "2.1.0 Sender %s accepted", displayPath(addr))
}

func (s *session) handleRcpt(args string) {
	if !s.inMail {
		s.reply(503, "5.5.1 Send MAIL FROM first")
		return
	}
	rest, ok := cutPrefixFold(args, "TO:")
	if !ok {
		s.reply(501, "5.5.4 Syntax: RCPT TO:<address>")
		return
	}
	addr, _, err := parsePath(rest)
	if err != nil {
		s.reply(501, "5.5.4 %s", err)
		return
	}
	if addr == "" {
		s.reply(501, "5.1.3 Empty recipient address")
		return
	}
	if len(s.rcpts) >= s.srv.opts.MaxRecipients {
		s.reply(452, "4.5.3 Too many recipients")
		return
	}
	s.rcpts = append(s.rcpts, addr)
	s.reply(250, "2.1.5 Recipient %s accepted", addr)
}

func (s *session) handleData() {
	if !s.inMail {
		s.reply(503, "5.5.1 Send MAIL FROM first")
		return
	}
	if len(s.rcpts) == 0 {
		s.reply(503, "5.5.1 Send RCPT TO first")
		return
	}
	s.reply(354, "End data with <CR><LF>.<CR><LF>")

	body, tooLarge, err := s.readData()
	if err != nil {
		return
	}
	if tooLarge {
		s.resetTransaction()
		s.reply(552, "5.3.4 Message exceeds the %d byte limit", s.srv.opts.MaxSize)
		return
	}

	mailboxName := s.srv.resolveMailbox(s.username, body)
	if s.srv.opts.AddReceived {
		body = append(s.receivedHeader(), body...)
	}

	msg, err := s.srv.opts.Store.Deliver(mailboxName, "smtp", store.Envelope{
		From:       s.from,
		To:         append([]string(nil), s.rcpts...),
		RemoteAddr: s.remoteAddr(),
		Username:   s.username,
		TLS:        s.tlsActive,
	}, body)
	s.resetTransaction()
	if err != nil {
		if errors.Is(err, store.ErrTooLarge) {
			s.reply(552, "5.3.4 Message exceeds the %d byte limit", s.srv.opts.MaxSize)
			return
		}
		s.srv.opts.Logger.Error("smtp delivery failed", "error", err)
		s.reply(451, "4.3.0 Cannot store message: %v", err)
		return
	}
	s.srv.opts.Logger.Info("message accepted",
		"protocol", "smtp", "mailbox", mailboxName, "id", msg.ID,
		"from", msg.Envelope.From, "recipients", len(msg.Envelope.To), "bytes", msg.Size)
	s.reply(250, "2.0.0 OK: queued as %s in mailbox %s", msg.ID, mailboxName)
}

// readData reads the DATA payload, undoing dot-stuffing and normalising line
// endings to CRLF. Oversized messages are drained to keep the session in sync.
func (s *session) readData() (body []byte, tooLarge bool, err error) {
	buf := make([]byte, 0, 8192)
	for {
		line, err := s.readLine()
		if err != nil {
			return nil, false, err
		}
		if line == "." {
			return buf, tooLarge, nil
		}
		if strings.HasPrefix(line, ".") {
			line = line[1:]
		}
		if int64(len(buf))+int64(len(line))+2 > s.srv.opts.MaxSize {
			tooLarge = true
			continue
		}
		if !tooLarge {
			buf = append(buf, line...)
			buf = append(buf, '\r', '\n')
		}
	}
}

// receivedHeader documents the hop the way a real MTA would, which makes the
// stored .eml files realistic to debug against.
func (s *session) receivedHeader() []byte {
	proto := "SMTP"
	if s.helo != "" {
		proto = "ESMTP"
	}
	if s.tlsActive {
		proto += "S"
	}
	if s.authenticated {
		proto += "A"
	}
	return []byte(fmt.Sprintf("Received: from %s (%s)\r\n\tby %s (Mailschleuse) with %s;\r\n\t%s\r\n",
		orDefault(s.helo, "unknown"), s.remoteAddr(), s.srv.opts.Hostname, proto,
		time.Now().Format(time.RFC1123Z)))
}

// resolveMailbox decides where a message is stored: an explicit routing header
// wins, then the authenticated user name, then the configured default.
func (s *Server) resolveMailbox(username string, body []byte) string {
	if s.opts.HeaderRouting {
		if name := headerValue(body, RoutingHeader); name != "" {
			candidate := strings.ToLower(strings.TrimSpace(name))
			if s.opts.Store.Has(candidate) {
				return candidate
			}
		}
	}
	if s.opts.UserRouting && username != "" {
		candidate := strings.ToLower(strings.TrimSpace(username))
		if s.opts.Store.Has(candidate) {
			return candidate
		}
	}
	return s.opts.DefaultMailbox
}

// headerValue extracts a header from the raw message head without parsing the
// whole MIME tree.
func headerValue(body []byte, name string) string {
	prefix := strings.ToLower(name) + ":"
	text := string(body)
	if end := strings.Index(text, "\r\n\r\n"); end >= 0 {
		text = text[:end]
	}
	for _, line := range strings.Split(text, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func (s *session) resetTransaction() {
	s.from = ""
	s.rcpts = nil
	s.inMail = false
}

func (s *session) remoteAddr() string {
	if s.conn == nil || s.conn.RemoteAddr() == nil {
		return ""
	}
	return s.conn.RemoteAddr().String()
}

// readLine reads one CRLF terminated line without its terminator.
func (s *session) readLine() (string, error) {
	s.conn.SetReadDeadline(time.Now().Add(s.srv.opts.IdleTimeout))
	var sb strings.Builder
	for {
		chunk, isPrefix, err := s.r.ReadLine()
		if err != nil {
			return "", err
		}
		if sb.Len()+len(chunk) > maxLineBytes {
			return "", io.ErrShortBuffer
		}
		sb.Write(chunk)
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

func (s *session) reply(code int, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(s.w, "%d %s\r\n", code, text)
	s.w.Flush()
}

func (s *session) multiReply(code int, lines []string) {
	s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	for i, line := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		fmt.Fprintf(s.w, "%d%s%s\r\n", code, sep, line)
	}
	s.w.Flush()
}

func splitCommand(line string) (string, string) {
	line = strings.TrimSpace(line)
	verb, rest, _ := strings.Cut(line, " ")
	return strings.ToUpper(verb), strings.TrimSpace(rest)
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(s[len(prefix):]), true
}

// parsePath parses "<addr> KEY=VALUE ..." as used by MAIL and RCPT.
func parsePath(raw string) (string, map[string]string, error) {
	raw = strings.TrimSpace(raw)
	params := map[string]string{}
	if !strings.HasPrefix(raw, "<") {
		// Tolerate clients that omit the angle brackets.
		addr, rest, _ := strings.Cut(raw, " ")
		parseParams(rest, params)
		return strings.TrimSpace(addr), params, nil
	}
	end := strings.Index(raw, ">")
	if end < 0 {
		return "", nil, errors.New("Syntax: address must be enclosed in <>")
	}
	addr := raw[1:end]
	parseParams(raw[end+1:], params)
	return strings.TrimSpace(addr), params, nil
}

func parseParams(raw string, into map[string]string) {
	for _, field := range strings.Fields(raw) {
		key, value, _ := strings.Cut(field, "=")
		into[strings.ToUpper(key)] = value
	}
}

func displayPath(addr string) string {
	if addr == "" {
		return "<>"
	}
	return addr
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
