package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/mailparse"
)

// persistence mirrors the in-memory mailboxes onto disk so a restart does not
// lose messages. The layout is deliberately boring - one .eml with the verbatim
// bytes plus one .json with the envelope metadata per message - so the data
// directory stays useful with nothing but a text editor.
type persistence struct {
	dir string
}

type metaFile struct {
	ID         string    `json:"id"`
	Mailbox    string    `json:"mailbox"`
	ReceivedAt time.Time `json:"receivedAt"`
	Source     string    `json:"source"`
	Envelope   Envelope  `json:"envelope"`
	Read       bool      `json:"read"`
}

func newPersistence(dir string, mailboxes []string) (*persistence, error) {
	for _, name := range mailboxes {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o750); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	return &persistence{dir: dir}, nil
}

func (p *persistence) pathFor(m *Message, ext string) string {
	return filepath.Join(p.dir, m.Mailbox, m.ID+ext)
}

// write stores (or updates) a message on disk. The raw body is only written
// once; later calls just refresh the metadata.
func (p *persistence) write(m *Message) error {
	emlPath := p.pathFor(m, ".eml")
	if _, err := os.Stat(emlPath); err != nil {
		if err := writeFileAtomic(emlPath, m.raw); err != nil {
			return fmt.Errorf("persist message body: %w", err)
		}
	}
	meta, err := json.Marshal(metaFile{
		ID:         m.ID,
		Mailbox:    m.Mailbox,
		ReceivedAt: m.ReceivedAt,
		Source:     m.Source,
		Envelope:   m.Envelope,
		Read:       m.read.Load(),
	})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.pathFor(m, ".json"), meta); err != nil {
		return fmt.Errorf("persist message metadata: %w", err)
	}
	return nil
}

func (p *persistence) remove(m *Message) {
	os.Remove(p.pathFor(m, ".eml"))
	os.Remove(p.pathFor(m, ".json"))
}

// writeFileAtomic writes through a temporary file so a crash mid-write cannot
// leave a half-written message behind.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// load restores persisted messages into the in-memory mailboxes, keeping only
// the newest MaxMessages per mailbox and discarding the rest from disk.
func (s *Store) load() error {
	for _, name := range s.order {
		dir := filepath.Join(s.persist.dir, name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read data directory: %w", err)
		}

		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
		// IDs carry a millisecond prefix, so sorting restores delivery order.
		sort.Strings(ids)

		box := s.boxes[name]
		for _, id := range ids {
			msg, err := s.persist.read(name, id)
			if err != nil {
				// A damaged message must not stop the service from starting.
				continue
			}
			box.messages = append(box.messages, msg)
			s.byID[msg.ID] = msg
		}
		for _, evicted := range s.evictLocked(box) {
			s.persist.remove(evicted)
		}
	}
	return nil
}

func (p *persistence) read(mailbox, id string) (*Message, error) {
	metaRaw, err := os.ReadFile(filepath.Join(p.dir, mailbox, id+".json"))
	if err != nil {
		return nil, err
	}
	var meta metaFile
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(p.dir, mailbox, id+".eml"))
	if err != nil {
		return nil, err
	}
	if meta.Envelope.To == nil {
		meta.Envelope.To = []string{}
	}
	msg := &Message{
		ID:         id,
		Mailbox:    mailbox,
		ReceivedAt: meta.ReceivedAt,
		Source:     meta.Source,
		Envelope:   meta.Envelope,
		Size:       len(raw),
		raw:        raw,
		parsed:     mailparse.Parse(raw),
	}
	msg.read.Store(meta.Read)
	return msg, nil
}
