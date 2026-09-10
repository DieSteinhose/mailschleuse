package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Mailboxes) != 2 || cfg.Mailboxes[0] != "inbox" || cfg.Mailboxes[1] != "outbox" {
		t.Errorf("mailboxes = %v", cfg.Mailboxes)
	}
	if cfg.SMTPMailbox != "outbox" || cfg.POP3Mailbox != "inbox" {
		t.Errorf("default routing: smtp=%s pop3=%s", cfg.SMTPMailbox, cfg.POP3Mailbox)
	}
	if cfg.SMTPAddr != ":1025" || cfg.POP3Addr != ":1110" || cfg.HTTPAddr != ":8080" {
		t.Errorf("addresses: %s %s %s", cfg.HTTPAddr, cfg.SMTPAddr, cfg.POP3Addr)
	}
	if cfg.MaxSizeBytes != 25*1024*1024 || cfg.MaxMessages != 500 {
		t.Errorf("limits: %d bytes, %d messages", cfg.MaxSizeBytes, cfg.MaxMessages)
	}
	if cfg.TLSMode != TLSAuto || cfg.SMTPAuth != AuthOptional {
		t.Errorf("tls=%s auth=%s", cfg.TLSMode, cfg.SMTPAuth)
	}
}

func TestByteSizes(t *testing.T) {
	for value, want := range map[string]int64{
		"1024": 1024,
		"32KB": 32 * 1024,
		"25MB": 25 * 1024 * 1024,
		"1GB":  1024 * 1024 * 1024,
		"2m":   2 * 1024 * 1024,
	} {
		t.Setenv("MS_MAX_MESSAGE_SIZE", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if cfg.MaxSizeBytes != want {
			t.Errorf("%s = %d bytes, want %d", value, cfg.MaxSizeBytes, want)
		}
	}
}

func TestDurationsAndBooleans(t *testing.T) {
	t.Setenv("MS_IDLE_TIMEOUT", "45s")
	t.Setenv("MS_POP3_EXCLUSIVE_LOCK", "no")
	t.Setenv("MS_SMTP_HEADER_ROUTING", "off")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.IdleTimeout != 45*time.Second {
		t.Errorf("idle timeout = %v", cfg.IdleTimeout)
	}
	if cfg.POP3Lock || cfg.SMTPHeaderRoute {
		t.Errorf("booleans not applied: lock=%v headerRouting=%v", cfg.POP3Lock, cfg.SMTPHeaderRoute)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	cases := map[string][2]string{
		"unknown smtp mailbox":  {"MS_SMTP_MAILBOX", "nowhere"},
		"unknown pop3 mailbox":  {"MS_POP3_MAILBOX", "nowhere"},
		"bad mailbox name":      {"MS_MAILBOXES", "Inbox Mail"},
		"duplicate mailbox":     {"MS_MAILBOXES", "inbox,inbox"},
		"bad boolean":           {"MS_POP3_EXCLUSIVE_LOCK", "maybe"},
		"bad number":            {"MS_MAX_MESSAGES", "many"},
		"bad duration":          {"MS_IDLE_TIMEOUT", "soon"},
		"bad auth mode":         {"MS_SMTP_AUTH", "sometimes"},
		"bad tls mode":          {"MS_TLS_MODE", "perhaps"},
		"too small size limit":  {"MS_MAX_MESSAGE_SIZE", "10"},
		"password without user": {"MS_SMTP_USERNAME", "dev"},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if _, err := Load(); err == nil {
				t.Errorf("%s=%s was accepted", kv[0], kv[1])
			}
		})
	}
}

func TestAuthRequiredNeedsCredentials(t *testing.T) {
	t.Setenv("MS_SMTP_AUTH", "required")
	if _, err := Load(); err == nil {
		t.Fatal("auth=required without credentials should fail")
	}
	t.Setenv("MS_SMTP_USERNAME", "dev")
	t.Setenv("MS_SMTP_PASSWORD", "secret")
	if _, err := Load(); err != nil {
		t.Fatalf("auth=required with credentials: %v", err)
	}
}

func TestImplicitTLSNeedsCertificate(t *testing.T) {
	t.Setenv("MS_TLS_MODE", "off")
	t.Setenv("MS_SMTP_TLS_ADDR", ":465")
	if _, err := Load(); err == nil {
		t.Fatal("implicit TLS without a certificate should fail")
	}
}

func TestCustomMailboxes(t *testing.T) {
	t.Setenv("MS_MAILBOXES", "team-a,team_b,archive")
	t.Setenv("MS_SMTP_MAILBOX", "archive")
	t.Setenv("MS_POP3_MAILBOX", "team-a")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Mailboxes) != 3 || !cfg.HasMailbox("team_b") {
		t.Errorf("mailboxes = %v", cfg.Mailboxes)
	}
}

func TestBasePathNormalisation(t *testing.T) {
	for value, want := range map[string]string{
		"":            "/",
		"/":           "/",
		"mail":        "/mail",
		"/mail/":      "/mail",
		"//mail//sub": "/mail//sub",
	} {
		if got := normalizeBasePath(value); got != want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestValidMailboxName(t *testing.T) {
	valid := []string{"inbox", "out-box", "team_1", "a"}
	invalid := []string{"", "Inbox", "with space", "sehr-langer-name-der-viel-zu-lang-ist-fuer-eine-mailbox", "mail/box"}
	for _, name := range valid {
		if !ValidMailboxName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range invalid {
		if ValidMailboxName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}
