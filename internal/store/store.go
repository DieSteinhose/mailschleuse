// Package store keeps the delivered messages. Every mailbox is an independent,
// size-capped queue: mail that an application submits over SMTP and mail that a
// developer injects for the application to fetch never mix, which is what keeps
// a send/receive test from looping back on itself.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/mailparse"
)

// ErrNoSuchMailbox is returned when an operation names an unknown mailbox.
var ErrNoSuchMailbox = errors.New("no such mailbox")

// ErrTooLarge is returned when a message exceeds the configured size limit.
var ErrTooLarge = errors.New("message too large")

// Envelope holds the SMTP level delivery information, which can differ from the
// From/To headers inside the message body.
type Envelope struct {
	From       string   `json:"from"`
	To         []string `json:"to"`
	RemoteAddr string   `json:"remoteAddr,omitempty"`
	Username   string   `json:"username,omitempty"`
	TLS        bool     `json:"tls"`
}

// Message is one stored mail together with how it arrived.
type Message struct {
	ID         string    `json:"id"`
	Mailbox    string    `json:"mailbox"`
	ReceivedAt time.Time `json:"receivedAt"`
	Source     string    `json:"source"`
	Envelope   Envelope  `json:"envelope"`
	Size       int       `json:"size"`

	// read is atomic because the web API, the message list and the POP3
	// session can all touch it concurrently.
	read   atomic.Bool
	raw    []byte
	parsed *mailparse.Message
}

// Read reports whether the message has been opened in the UI.
func (m *Message) Read() bool { return m.read.Load() }

// Raw returns the verbatim bytes as they were delivered.
func (m *Message) Raw() []byte { return m.raw }

// Parsed returns the decoded view of the message.
func (m *Message) Parsed() *mailparse.Message { return m.parsed }

// Summary is the compact representation used for mailbox listings.
type Summary struct {
	ID          string              `json:"id"`
	Mailbox     string              `json:"mailbox"`
	ReceivedAt  time.Time           `json:"receivedAt"`
	Source      string              `json:"source"`
	Size        int                 `json:"size"`
	Read        bool                `json:"read"`
	Subject     string              `json:"subject"`
	From        []mailparse.Address `json:"from"`
	To          []mailparse.Address `json:"to"`
	Snippet     string              `json:"snippet"`
	HasHTML     bool                `json:"hasHtml"`
	Attachments int                 `json:"attachments"`
	Envelope    Envelope            `json:"envelope"`
}

// Summary condenses the message for list views.
func (m *Message) Summary() Summary {
	p := m.parsed
	s := Summary{
		ID:         m.ID,
		Mailbox:    m.Mailbox,
		ReceivedAt: m.ReceivedAt,
		Source:     m.Source,
		Size:       m.Size,
		Read:       m.read.Load(),
		Envelope:   m.Envelope,
		From:       []mailparse.Address{},
		To:         []mailparse.Address{},
	}
	if p != nil {
		s.Subject = p.Subject
		s.From = p.From
		s.To = p.To
		s.HasHTML = p.HTML != ""
		s.Attachments = len(p.Attachments)
		s.Snippet = snippet(p)
	}
	if len(s.From) == 0 && m.Envelope.From != "" {
		s.From = []mailparse.Address{{Address: m.Envelope.From}}
	}
	if len(s.To) == 0 && len(m.Envelope.To) > 0 {
		for _, rcpt := range m.Envelope.To {
			s.To = append(s.To, mailparse.Address{Address: rcpt})
		}
	}
	return s
}

// MailboxStats describes a mailbox for the navigation sidebar.
type MailboxStats struct {
	Name   string `json:"name"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
	Bytes  int64  `json:"bytes"`
}

// EventType names a change that listeners can react to.
type EventType string

const (
	// EventNew is emitted after a message was stored.
	EventNew EventType = "message.new"
	// EventUpdated is emitted when message metadata such as the read flag changed.
	EventUpdated EventType = "message.updated"
	// EventDeleted is emitted after a message was removed.
	EventDeleted EventType = "message.deleted"
	// EventCleared is emitted after a whole mailbox was emptied.
	EventCleared EventType = "mailbox.cleared"
)

// Event describes a single store change.
type Event struct {
	Type    EventType `json:"type"`
	Mailbox string    `json:"mailbox"`
	ID      string    `json:"id,omitempty"`
	Summary *Summary  `json:"message,omitempty"`
}

// Options configures a Store.
type Options struct {
	Mailboxes   []string
	MaxMessages int
	MaxSize     int64
	DataDir     string
}

type mailbox struct {
	name     string
	messages []*Message
}

// Store is a set of mailboxes plus the change notification fan-out.
type Store struct {
	mu          sync.RWMutex
	boxes       map[string]*mailbox
	order       []string
	byID        map[string]*Message
	maxMessages int
	maxSize     int64

	persist *persistence

	subMu sync.Mutex
	subs  map[int]chan Event
	nexID int
}

// New builds a store for the given mailboxes, restoring persisted messages when
// a data directory is configured.
func New(opts Options) (*Store, error) {
	s := &Store{
		boxes:       make(map[string]*mailbox, len(opts.Mailboxes)),
		byID:        make(map[string]*Message),
		maxMessages: opts.MaxMessages,
		maxSize:     opts.MaxSize,
		subs:        make(map[int]chan Event),
	}
	for _, name := range opts.Mailboxes {
		s.boxes[name] = &mailbox{name: name}
		s.order = append(s.order, name)
	}
	if opts.DataDir != "" {
		p, err := newPersistence(opts.DataDir, opts.Mailboxes)
		if err != nil {
			return nil, err
		}
		s.persist = p
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Mailboxes returns the configured mailbox names in configuration order.
func (s *Store) Mailboxes() []string {
	return append([]string(nil), s.order...)
}

// Has reports whether the named mailbox exists.
func (s *Store) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.boxes[name]
	return ok
}

// Stats returns per-mailbox counters in configuration order.
func (s *Store) Stats() []MailboxStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]MailboxStats, 0, len(s.order))
	for _, name := range s.order {
		box := s.boxes[name]
		st := MailboxStats{Name: name, Total: len(box.messages)}
		for _, m := range box.messages {
			if !m.read.Load() {
				st.Unread++
			}
			st.Bytes += int64(m.Size)
		}
		out = append(out, st)
	}
	return out
}

// Deliver stores raw into the named mailbox and returns the stored message.
func (s *Store) Deliver(mailboxName, source string, env Envelope, raw []byte) (*Message, error) {
	if int64(len(raw)) > s.maxSize {
		return nil, ErrTooLarge
	}

	msg := &Message{
		ID:         NewID(),
		Mailbox:    mailboxName,
		ReceivedAt: time.Now().UTC(),
		Source:     source,
		Envelope:   env,
		Size:       len(raw),
		raw:        raw,
		parsed:     mailparse.Parse(raw),
	}
	if msg.Envelope.To == nil {
		msg.Envelope.To = []string{}
	}

	s.mu.Lock()
	box, ok := s.boxes[mailboxName]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNoSuchMailbox, mailboxName)
	}
	box.messages = append(box.messages, msg)
	s.byID[msg.ID] = msg
	evicted := s.evictLocked(box)
	s.mu.Unlock()

	if s.persist != nil {
		if err := s.persist.write(msg); err != nil {
			return nil, err
		}
		for _, old := range evicted {
			s.persist.remove(old)
		}
	}

	for _, old := range evicted {
		s.publish(Event{Type: EventDeleted, Mailbox: old.Mailbox, ID: old.ID})
	}
	summary := msg.Summary()
	s.publish(Event{Type: EventNew, Mailbox: mailboxName, ID: msg.ID, Summary: &summary})
	return msg, nil
}

// evictLocked trims a mailbox back to the configured capacity. The caller holds
// the write lock.
func (s *Store) evictLocked(box *mailbox) []*Message {
	if s.maxMessages <= 0 || len(box.messages) <= s.maxMessages {
		return nil
	}
	drop := len(box.messages) - s.maxMessages
	evicted := make([]*Message, drop)
	copy(evicted, box.messages[:drop])
	box.messages = append([]*Message(nil), box.messages[drop:]...)
	for _, m := range evicted {
		delete(s.byID, m.ID)
	}
	return evicted
}

// ListOptions filters and pages a mailbox listing.
type ListOptions struct {
	Mailbox string
	Search  string
	Limit   int
	Offset  int
	Unread  bool
}

// List returns newest-first summaries for a mailbox plus the number of matches.
func (s *Store) List(opts ListOptions) ([]Summary, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pool []*Message
	if opts.Mailbox == "" {
		for _, name := range s.order {
			pool = append(pool, s.boxes[name].messages...)
		}
		sort.SliceStable(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })
	} else {
		box, ok := s.boxes[opts.Mailbox]
		if !ok {
			return nil, 0, fmt.Errorf("%w: %s", ErrNoSuchMailbox, opts.Mailbox)
		}
		pool = box.messages
	}

	needle := strings.ToLower(strings.TrimSpace(opts.Search))
	matches := make([]*Message, 0, len(pool))
	for _, m := range pool {
		if opts.Unread && m.read.Load() {
			continue
		}
		if needle != "" && !m.matches(needle) {
			continue
		}
		matches = append(matches, m)
	}

	total := len(matches)
	limit := opts.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	// Newest first: walk the append-ordered slice backwards.
	out := make([]Summary, 0, limit)
	for i := total - 1 - opts.Offset; i >= 0 && len(out) < limit; i-- {
		out = append(out, matches[i].Summary())
	}
	return out, total, nil
}

// matches reports whether the message contains needle in a searchable field.
func (m *Message) matches(needle string) bool {
	if strings.Contains(strings.ToLower(m.Envelope.From), needle) {
		return true
	}
	for _, rcpt := range m.Envelope.To {
		if strings.Contains(strings.ToLower(rcpt), needle) {
			return true
		}
	}
	p := m.parsed
	if p == nil {
		return false
	}
	if strings.Contains(strings.ToLower(p.Subject), needle) ||
		strings.Contains(strings.ToLower(p.Text), needle) {
		return true
	}
	for _, group := range [][]mailparse.Address{p.From, p.To, p.Cc} {
		for _, a := range group {
			if strings.Contains(strings.ToLower(a.String()), needle) {
				return true
			}
		}
	}
	return false
}

// Get returns a single message by ID.
func (s *Store) Get(id string) (*Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.byID[id]
	return m, ok
}

// Snapshot returns the current messages of a mailbox oldest-first. POP3 uses it
// to freeze the mailbox contents for the duration of a session.
func (s *Store) Snapshot(mailboxName string) ([]*Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	box, ok := s.boxes[mailboxName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchMailbox, mailboxName)
	}
	return append([]*Message(nil), box.messages...), nil
}

// MarkRead sets or clears the read flag of a message.
func (s *Store) MarkRead(id string, read bool) bool {
	s.mu.RLock()
	m, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return false
	}

	if changed := m.read.Swap(read) != read; changed {
		if s.persist != nil {
			s.persist.write(m)
		}
		summary := m.Summary()
		s.publish(Event{Type: EventUpdated, Mailbox: m.Mailbox, ID: m.ID, Summary: &summary})
	}
	return true
}

// MarkAllRead marks every message of a mailbox as read and returns the count.
func (s *Store) MarkAllRead(mailboxName string) (int, error) {
	s.mu.Lock()
	box, ok := s.boxes[mailboxName]
	if !ok {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrNoSuchMailbox, mailboxName)
	}
	var changed []*Message
	for _, m := range box.messages {
		if !m.read.Swap(true) {
			changed = append(changed, m)
		}
	}
	s.mu.Unlock()

	for _, m := range changed {
		if s.persist != nil {
			s.persist.write(m)
		}
		summary := m.Summary()
		s.publish(Event{Type: EventUpdated, Mailbox: m.Mailbox, ID: m.ID, Summary: &summary})
	}
	return len(changed), nil
}

// Delete removes a single message.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	m, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.byID, id)
	box := s.boxes[m.Mailbox]
	for i, candidate := range box.messages {
		if candidate.ID == id {
			box.messages = append(box.messages[:i], box.messages[i+1:]...)
			break
		}
	}
	s.mu.Unlock()

	if s.persist != nil {
		s.persist.remove(m)
	}
	s.publish(Event{Type: EventDeleted, Mailbox: m.Mailbox, ID: id})
	return true
}

// Clear empties a mailbox and returns how many messages were removed.
func (s *Store) Clear(mailboxName string) (int, error) {
	s.mu.Lock()
	box, ok := s.boxes[mailboxName]
	if !ok {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrNoSuchMailbox, mailboxName)
	}
	removed := box.messages
	box.messages = nil
	for _, m := range removed {
		delete(s.byID, m.ID)
	}
	s.mu.Unlock()

	if s.persist != nil {
		for _, m := range removed {
			s.persist.remove(m)
		}
	}
	s.publish(Event{Type: EventCleared, Mailbox: mailboxName})
	return len(removed), nil
}

// Subscribe registers a listener for store events. The returned function
// unsubscribes and closes the channel.
func (s *Store) Subscribe() (<-chan Event, func()) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	id := s.nexID
	s.nexID++
	ch := make(chan Event, 64)
	s.subs[id] = ch
	return ch, func() {
		s.subMu.Lock()
		defer s.subMu.Unlock()
		if existing, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(existing)
		}
	}
}

// publish fans an event out to all listeners, dropping events for listeners
// that cannot keep up rather than blocking mail delivery.
func (s *Store) publish(ev Event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// snippet builds a short preview line from the message body.
func snippet(p *mailparse.Message) string {
	source := p.Text
	if source == "" {
		source = stripTags(p.HTML)
	}
	source = strings.Join(strings.Fields(source), " ")
	if len(source) > 180 {
		// Cut on a rune boundary so the JSON stays valid UTF-8.
		cut := 180
		for cut > 0 && !isRuneStart(source[cut]) {
			cut--
		}
		source = strings.TrimSpace(source[:cut]) + "..."
	}
	return source
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// stripTags removes HTML markup for preview purposes only.
func stripTags(html string) string {
	var b strings.Builder
	depth := 0
	for _, r := range html {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NewID returns a lexicographically sortable, collision-resistant message ID.
// The millisecond prefix keeps directory listings in delivery order.
func NewID() string {
	var buf [6]byte
	rand.Read(buf[:])
	return fmt.Sprintf("%011x%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(buf[:]))
}
