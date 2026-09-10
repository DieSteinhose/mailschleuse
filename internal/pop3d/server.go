// Package pop3d implements the POP3 endpoint an application polls to pick up
// mail. It follows RFC 1939 including the session snapshot semantics, so a
// client that deletes messages after fetching them behaves exactly as it would
// against a production mail server.
package pop3d

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/store"
)

// Options configures the POP3 server.
type Options struct {
	Hostname       string
	Username       string
	Password       string
	DefaultMailbox string
	UserRouting    bool
	ExclusiveLock  bool
	IdleTimeout    time.Duration
	TLSConfig      *tls.Config
	Store          *store.Store
	Logger         *slog.Logger
}

// Server accepts POP3 connections until it is closed.
type Server struct {
	opts Options

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	locks     map[string]struct{}
	closed    bool
	wg        sync.WaitGroup
}

// New creates a server from the given options.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Server{
		opts:  opts,
		conns: make(map[net.Conn]struct{}),
		locks: make(map[string]struct{}),
	}
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

// lock implements the exclusive-access rule of RFC 1939 section 3.
func (s *Server) lock(mailbox string) bool {
	if !s.opts.ExclusiveLock {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.locks[mailbox]; taken {
		return false
	}
	s.locks[mailbox] = struct{}{}
	return true
}

func (s *Server) unlock(mailbox string) {
	if !s.opts.ExclusiveLock {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locks, mailbox)
}

// entry is one message in the frozen session view.
type entry struct {
	msg     *store.Message
	deleted bool
}

type session struct {
	srv  *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	tlsActive   bool
	username    string
	mailbox     string
	locked      bool
	transaction bool
	entries     []*entry
}

func (s *Server) handle(conn net.Conn) {
	sess := &session{
		srv:  s,
		conn: conn,
		r:    bufio.NewReaderSize(conn, 4096),
		w:    bufio.NewWriterSize(conn, 32*1024),
	}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		sess.tlsActive = true
	}
	defer sess.release()
	sess.run()
}

func (s *session) release() {
	if s.locked {
		s.srv.unlock(s.mailbox)
		s.locked = false
	}
}

func (s *session) run() {
	s.ok("Mailschleuse POP3 ready on %s", s.srv.opts.Hostname)
	for {
		line, err := s.readLine()
		if err != nil {
			return
		}
		verb, args := splitCommand(line)
		switch verb {
		case "CAPA":
			s.handleCapa()
		case "STLS":
			if !s.handleSTLS() {
				return
			}
		case "USER":
			s.handleUser(args)
		case "PASS":
			s.handlePass(args)
		case "STAT":
			s.handleStat()
		case "LIST":
			s.handleList(args, false)
		case "UIDL":
			s.handleList(args, true)
		case "RETR":
			s.handleRetr(args)
		case "TOP":
			s.handleTop(args)
		case "DELE":
			s.handleDele(args)
		case "RSET":
			s.handleRset()
		case "NOOP":
			s.ok("")
		case "QUIT":
			s.handleQuit()
			return
		default:
			s.err("unknown command %q", verb)
		}
	}
}

func (s *session) handleCapa() {
	caps := []string{"TOP", "UIDL", "USER", "RESP-CODES", "PIPELINING", "IMPLEMENTATION Mailschleuse"}
	if s.srv.opts.TLSConfig != nil && !s.tlsActive {
		caps = append([]string{"STLS"}, caps...)
	}
	s.ok("Capability list follows")
	for _, c := range caps {
		s.line("%s", c)
	}
	s.line(".")
	s.flush()
}

func (s *session) handleSTLS() bool {
	if s.srv.opts.TLSConfig == nil {
		s.err("TLS not available")
		return true
	}
	if s.tlsActive {
		s.err("TLS already active")
		return true
	}
	s.ok("Begin TLS negotiation")
	tlsConn := tls.Server(s.conn, s.srv.opts.TLSConfig)
	s.conn.SetDeadline(time.Now().Add(s.srv.opts.IdleTimeout))
	if err := tlsConn.Handshake(); err != nil {
		s.srv.opts.Logger.Debug("pop3 stls handshake failed", "error", err)
		return false
	}
	s.conn = tlsConn
	s.r = bufio.NewReaderSize(tlsConn, 4096)
	s.w = bufio.NewWriterSize(tlsConn, 32*1024)
	s.tlsActive = true
	s.username = ""
	return true
}

func (s *session) handleUser(args string) {
	if s.transaction {
		s.err("already authenticated")
		return
	}
	if strings.TrimSpace(args) == "" {
		s.err("USER requires a name")
		return
	}
	s.username = strings.TrimSpace(args)
	s.ok("user accepted, send PASS")
}

func (s *session) handlePass(args string) {
	if s.transaction {
		s.err("already authenticated")
		return
	}
	if s.username == "" {
		s.err("send USER first")
		return
	}
	if !s.srv.credentialsValid(s.username, args) {
		s.srv.opts.Logger.Info("pop3 auth rejected", "user", s.username, "remote", s.conn.RemoteAddr().String())
		s.err("invalid credentials")
		s.username = ""
		return
	}

	s.mailbox = s.srv.resolveMailbox(s.username)
	if !s.srv.lock(s.mailbox) {
		s.err("[IN-USE] mailbox %s is locked by another session", s.mailbox)
		s.username = ""
		return
	}
	s.locked = true

	messages, err := s.srv.opts.Store.Snapshot(s.mailbox)
	if err != nil {
		s.release()
		s.err("cannot open mailbox %s", s.mailbox)
		return
	}
	s.entries = make([]*entry, 0, len(messages))
	for _, m := range messages {
		s.entries = append(s.entries, &entry{msg: m})
	}
	s.transaction = true

	count, size := s.stats()
	s.srv.opts.Logger.Info("pop3 session opened", "mailbox", s.mailbox, "user", s.username, "messages", count)
	s.ok("mailbox %s ready: %d messages (%d octets)", s.mailbox, count, size)
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

// resolveMailbox maps the login name to a mailbox when user routing is enabled.
func (s *Server) resolveMailbox(username string) string {
	if s.opts.UserRouting {
		candidate := strings.ToLower(strings.TrimSpace(username))
		if s.opts.Store.Has(candidate) {
			return candidate
		}
	}
	return s.opts.DefaultMailbox
}

func (s *session) stats() (count int, size int) {
	for _, e := range s.entries {
		if e.deleted {
			continue
		}
		count++
		size += crlfSize(e.msg.Raw())
	}
	return count, size
}

func (s *session) handleStat() {
	if !s.requireTransaction() {
		return
	}
	count, size := s.stats()
	s.ok("%d %d", count, size)
}

// handleList serves both LIST and UIDL, which differ only in the second column.
func (s *session) handleList(args string, uidl bool) {
	if !s.requireTransaction() {
		return
	}
	label := "scan listing"
	if uidl {
		label = "unique-id listing"
	}

	if strings.TrimSpace(args) != "" {
		e, index, err := s.lookup(args)
		if err != nil {
			s.err("%s", err)
			return
		}
		if uidl {
			s.ok("%d %s", index, e.msg.ID)
			return
		}
		s.ok("%d %d", index, crlfSize(e.msg.Raw()))
		return
	}

	s.ok("%s follows", label)
	for i, e := range s.entries {
		if e.deleted {
			continue
		}
		if uidl {
			s.line("%d %s", i+1, e.msg.ID)
			continue
		}
		s.line("%d %d", i+1, crlfSize(e.msg.Raw()))
	}
	s.line(".")
	s.flush()
}

func (s *session) handleRetr(args string) {
	if !s.requireTransaction() {
		return
	}
	e, _, err := s.lookup(args)
	if err != nil {
		s.err("%s", err)
		return
	}
	s.ok("%d octets", crlfSize(e.msg.Raw()))
	s.writeDotStuffed(e.msg.Raw(), -1)
	s.srv.opts.Logger.Info("pop3 message fetched", "mailbox", s.mailbox, "id", e.msg.ID)
}

func (s *session) handleTop(args string) {
	if !s.requireTransaction() {
		return
	}
	fields := strings.Fields(args)
	if len(fields) != 2 {
		s.err("TOP requires a message number and a line count")
		return
	}
	e, _, err := s.lookup(fields[0])
	if err != nil {
		s.err("%s", err)
		return
	}
	lines, err := strconv.Atoi(fields[1])
	if err != nil || lines < 0 {
		s.err("invalid line count")
		return
	}
	s.ok("top of message follows")
	s.writeDotStuffed(e.msg.Raw(), lines)
}

func (s *session) handleDele(args string) {
	if !s.requireTransaction() {
		return
	}
	e, index, err := s.lookup(args)
	if err != nil {
		s.err("%s", err)
		return
	}
	e.deleted = true
	s.ok("message %d marked for deletion", index)
}

func (s *session) handleRset() {
	if !s.requireTransaction() {
		return
	}
	for _, e := range s.entries {
		e.deleted = false
	}
	count, size := s.stats()
	s.ok("%d messages (%d octets)", count, size)
}

// handleQuit enters the UPDATE state: messages marked with DELE are removed
// from the store now, not earlier.
func (s *session) handleQuit() {
	removed := 0
	if s.transaction {
		for _, e := range s.entries {
			if e.deleted && s.srv.opts.Store.Delete(e.msg.ID) {
				removed++
			}
		}
	}
	s.release()
	if removed > 0 {
		s.srv.opts.Logger.Info("pop3 session closed", "mailbox", s.mailbox, "deleted", removed)
	}
	s.ok("Mailschleuse signing off (%d messages deleted)", removed)
}

func (s *session) requireTransaction() bool {
	if !s.transaction {
		s.err("authenticate first")
		return false
	}
	return true
}

// lookup resolves a message number from the frozen session view.
func (s *session) lookup(arg string) (*entry, int, error) {
	index, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return nil, 0, fmt.Errorf("invalid message number %q", strings.TrimSpace(arg))
	}
	if index < 1 || index > len(s.entries) {
		return nil, 0, fmt.Errorf("no such message: %d", index)
	}
	e := s.entries[index-1]
	if e.deleted {
		return nil, 0, fmt.Errorf("message %d is marked for deletion", index)
	}
	return e, index, nil
}

// writeDotStuffed writes a message body in POP3 multi-line form: CRLF endings,
// leading dots doubled and a terminating "." line. A non-negative bodyLines
// limits how much of the body after the header block is sent (TOP).
func (s *session) writeDotStuffed(raw []byte, bodyLines int) {
	s.conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	inHeaders := bodyLines >= 0
	emitted := 0

	for _, line := range splitLines(raw) {
		if inHeaders && len(line) == 0 {
			inHeaders = false
			s.writeLine(nil)
			if bodyLines == 0 {
				break
			}
			continue
		}
		if !inHeaders && bodyLines >= 0 {
			if emitted >= bodyLines {
				break
			}
			emitted++
		}
		s.writeLine(line)
	}
	s.line(".")
	s.flush()
}

func (s *session) writeLine(line []byte) {
	if bytes.HasPrefix(line, []byte(".")) {
		s.w.WriteByte('.')
	}
	s.w.Write(line)
	s.w.WriteString("\r\n")
}

// splitLines splits on LF and drops a trailing CR, so both CRLF and LF sources
// yield the same logical lines.
func splitLines(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSuffix(raw, []byte("\n"))
	trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	lines := bytes.Split(trimmed, []byte("\n"))
	for i, line := range lines {
		lines[i] = bytes.TrimSuffix(line, []byte("\r"))
	}
	return lines
}

// crlfSize reports the octet count the client will actually receive, which is
// what STAT and LIST have to advertise.
func crlfSize(raw []byte) int {
	total := 0
	for _, line := range splitLines(raw) {
		total += len(line) + 2
		if bytes.HasPrefix(line, []byte(".")) {
			total++
		}
	}
	return total
}

func (s *session) readLine() (string, error) {
	s.conn.SetReadDeadline(time.Now().Add(s.srv.opts.IdleTimeout))
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (s *session) ok(format string, args ...any) {
	s.respond("+OK", format, args...)
}

func (s *session) err(format string, args ...any) {
	s.respond("-ERR", format, args...)
}

func (s *session) respond(prefix, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if text == "" {
		s.w.WriteString(prefix + "\r\n")
	} else {
		s.w.WriteString(prefix + " " + text + "\r\n")
	}
	s.flush()
}

func (s *session) line(format string, args ...any) {
	fmt.Fprintf(s.w, format+"\r\n", args...)
}

func (s *session) flush() { s.w.Flush() }

func splitCommand(line string) (string, string) {
	line = strings.TrimSpace(line)
	verb, rest, _ := strings.Cut(line, " ")
	return strings.ToUpper(verb), strings.TrimSpace(rest)
}
