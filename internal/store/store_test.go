package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := New(Options{
		Mailboxes:   []string{"inbox", "outbox"},
		MaxMessages: 3,
		MaxSize:     1 << 20,
		DataDir:     dir,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func rawMessage(subject, body string) []byte {
	return []byte("From: someone@example.com\r\nTo: dev@example.com\r\nSubject: " +
		subject + "\r\n\r\n" + body + "\r\n")
}

func TestDeliverAndList(t *testing.T) {
	s := newTestStore(t, "")

	if _, err := s.Deliver("inbox", "web", Envelope{From: "a@example.com"}, rawMessage("first", "hello")); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := s.Deliver("outbox", "smtp", Envelope{From: "b@example.com"}, rawMessage("second", "world")); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	list, total, err := s.List(ListOptions{Mailbox: "inbox"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("expected 1 message in inbox, got total=%d len=%d", total, len(list))
	}
	if list[0].Subject != "first" {
		t.Errorf("subject = %q, want %q", list[0].Subject, "first")
	}
	if list[0].Snippet != "hello" {
		t.Errorf("snippet = %q, want %q", list[0].Snippet, "hello")
	}

	// Mailboxes must stay separate: the outbox message may not show up here.
	if _, total, _ := s.List(ListOptions{Mailbox: "outbox"}); total != 1 {
		t.Errorf("outbox total = %d, want 1", total)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := newTestStore(t, "")
	for i := range 3 {
		if _, err := s.Deliver("inbox", "web", Envelope{}, rawMessage(fmt.Sprintf("msg-%d", i), "body")); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	list, _, err := s.List(ListOptions{Mailbox: "inbox"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list[0].Subject != "msg-2" || list[2].Subject != "msg-0" {
		t.Errorf("unexpected order: %q ... %q", list[0].Subject, list[2].Subject)
	}
}

func TestSearchFiltersMessages(t *testing.T) {
	s := newTestStore(t, "")
	s.Deliver("inbox", "web", Envelope{From: "alice@example.com"}, rawMessage("printer broken", "please help"))
	s.Deliver("inbox", "web", Envelope{From: "bob@example.com"}, rawMessage("holiday request", "next week"))

	list, total, err := s.List(ListOptions{Mailbox: "inbox", Search: "PRINTER"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || list[0].Subject != "printer broken" {
		t.Fatalf("search returned %d results, first %q", total, list[0].Subject)
	}

	if _, total, _ := s.List(ListOptions{Mailbox: "inbox", Search: "bob@"}); total != 1 {
		t.Errorf("envelope search total = %d, want 1", total)
	}
}

func TestCapacityEvictsOldest(t *testing.T) {
	s := newTestStore(t, "")
	for i := range 5 {
		s.Deliver("inbox", "web", Envelope{}, rawMessage(fmt.Sprintf("msg-%d", i), "body"))
	}
	list, total, _ := s.List(ListOptions{Mailbox: "inbox"})
	if total != 3 {
		t.Fatalf("total = %d, want 3 (the configured cap)", total)
	}
	if list[len(list)-1].Subject != "msg-2" {
		t.Errorf("oldest kept message = %q, want msg-2", list[len(list)-1].Subject)
	}
}

func TestDeleteAndClear(t *testing.T) {
	s := newTestStore(t, "")
	msg, _ := s.Deliver("inbox", "web", Envelope{}, rawMessage("one", "body"))
	s.Deliver("inbox", "web", Envelope{}, rawMessage("two", "body"))

	if !s.Delete(msg.ID) {
		t.Fatal("delete returned false for an existing message")
	}
	if _, ok := s.Get(msg.ID); ok {
		t.Error("message is still retrievable after delete")
	}
	if s.Delete(msg.ID) {
		t.Error("second delete of the same ID should fail")
	}

	removed, err := s.Clear("inbox")
	if err != nil || removed != 1 {
		t.Fatalf("clear = %d, %v; want 1, nil", removed, err)
	}
	if _, total, _ := s.List(ListOptions{Mailbox: "inbox"}); total != 0 {
		t.Errorf("mailbox not empty after clear")
	}
}

func TestReadFlags(t *testing.T) {
	s := newTestStore(t, "")
	msg, _ := s.Deliver("inbox", "web", Envelope{}, rawMessage("one", "body"))
	if stats := s.Stats()[0]; stats.Unread != 1 {
		t.Fatalf("unread = %d, want 1", stats.Unread)
	}
	s.MarkRead(msg.ID, true)
	if stats := s.Stats()[0]; stats.Unread != 0 {
		t.Fatalf("unread = %d, want 0", stats.Unread)
	}
	s.Deliver("inbox", "web", Envelope{}, rawMessage("two", "body"))
	if updated, _ := s.MarkAllRead("inbox"); updated != 1 {
		t.Errorf("MarkAllRead = %d, want 1", updated)
	}
}

func TestUnknownMailbox(t *testing.T) {
	s := newTestStore(t, "")
	if _, err := s.Deliver("nope", "web", Envelope{}, rawMessage("x", "y")); err == nil {
		t.Fatal("delivery into an unknown mailbox should fail")
	}
	if _, _, err := s.List(ListOptions{Mailbox: "nope"}); err == nil {
		t.Fatal("listing an unknown mailbox should fail")
	}
}

func TestOversizedMessageRejected(t *testing.T) {
	s, err := New(Options{Mailboxes: []string{"inbox"}, MaxMessages: 10, MaxSize: 32})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := s.Deliver("inbox", "web", Envelope{}, rawMessage("subject", "a long body that exceeds the limit")); err != ErrTooLarge {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestEventsArePublished(t *testing.T) {
	s := newTestStore(t, "")
	events, cancel := s.Subscribe()
	defer cancel()

	msg, _ := s.Deliver("inbox", "web", Envelope{}, rawMessage("one", "body"))
	ev := <-events
	if ev.Type != EventNew || ev.ID != msg.ID || ev.Mailbox != "inbox" {
		t.Fatalf("unexpected event %+v", ev)
	}

	s.Delete(msg.ID)
	if ev := <-events; ev.Type != EventDeleted {
		t.Fatalf("event type = %s, want %s", ev.Type, EventDeleted)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	first := newTestStore(t, dir)
	msg, err := first.Deliver("inbox", "smtp", Envelope{From: "a@example.com", To: []string{"b@example.com"}},
		rawMessage("persisted", "still here"))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	first.MarkRead(msg.ID, true)

	second := newTestStore(t, dir)
	restored, ok := second.Get(msg.ID)
	if !ok {
		t.Fatal("message was not restored from disk")
	}
	if !restored.Read() {
		t.Error("read flag was not restored")
	}
	if restored.Envelope.From != "a@example.com" {
		t.Errorf("envelope sender = %q", restored.Envelope.From)
	}
	if string(restored.Raw()) != string(msg.Raw()) {
		t.Error("raw message differs after restore")
	}

	// Deleting must remove the files, not just the in-memory copy.
	second.Delete(msg.ID)
	matches, _ := filepath.Glob(filepath.Join(dir, "inbox", "*.eml"))
	if len(matches) != 0 {
		t.Errorf("found %d leftover .eml files after delete", len(matches))
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	s := newTestStore(t, "")
	s.Deliver("inbox", "web", Envelope{}, rawMessage("one", "body"))
	snapshot, err := s.Snapshot("inbox")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	s.Deliver("inbox", "web", Envelope{}, rawMessage("two", "body"))
	if len(snapshot) != 1 {
		t.Errorf("snapshot grew to %d entries; it must stay frozen", len(snapshot))
	}
}

func TestConcurrentAccess(t *testing.T) {
	s, err := New(Options{Mailboxes: []string{"inbox"}, MaxMessages: 20, MaxSize: 1 << 20})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	events, cancel := s.Subscribe()
	defer cancel()
	go func() {
		for range events {
		}
	}()

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 25 {
				msg, err := s.Deliver("inbox", "web", Envelope{From: "a@example.com"},
					rawMessage(fmt.Sprintf("w%d-%d", worker, i), "body"))
				if err != nil {
					continue
				}
				s.MarkRead(msg.ID, true)
				s.List(ListOptions{Mailbox: "inbox"})
				s.Stats()
				s.Snapshot("inbox")
				if i%5 == 0 {
					s.Delete(msg.ID)
				}
			}
		}(worker)
	}
	wg.Wait()

	if _, total, err := s.List(ListOptions{Mailbox: "inbox"}); err != nil || total > 20 {
		t.Fatalf("total = %d (err %v), want at most the configured cap", total, err)
	}
}
