// Package config loads the Mailschleuse runtime configuration from the
// environment. Every knob is a MS_* environment variable so the whole service
// can be driven from a single .env file next to docker-compose.yml.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AuthMode describes how strict a protocol listener is about authentication.
type AuthMode string

const (
	// AuthDisabled rejects every AUTH attempt and never advertises AUTH.
	AuthDisabled AuthMode = "disabled"
	// AuthOptional accepts anonymous clients but validates credentials when offered.
	AuthOptional AuthMode = "optional"
	// AuthRequired refuses to accept mail before a successful AUTH.
	AuthRequired AuthMode = "required"
)

// TLSMode selects where the certificate for STARTTLS/implicit TLS comes from.
type TLSMode string

const (
	// TLSOff disables STARTTLS/STLS entirely.
	TLSOff TLSMode = "off"
	// TLSAuto generates a throwaway self-signed certificate on every start.
	TLSAuto TLSMode = "auto"
	// TLSFiles loads a certificate and key from disk.
	TLSFiles TLSMode = "files"
)

// Config is the fully resolved configuration of a Mailschleuse instance.
type Config struct {
	Hostname   string
	PublicHost string
	Mailboxes  []string

	// Storage
	DataDir      string
	MaxMessages  int
	MaxSizeBytes int64

	// HTTP
	HTTPAddr     string
	WebUsername  string
	WebPassword  string
	BasePath     string
	CORSOrigin   string
	ReadOnlyMode bool

	// SMTP
	SMTPAddr        string
	SMTPTLSAddr     string
	SMTPAuth        AuthMode
	SMTPUsername    string
	SMTPPassword    string
	SMTPMailbox     string
	SMTPUserRouting bool
	SMTPHeaderRoute bool
	SMTPAddReceived bool
	SMTPMaxRcpt     int

	// POP3
	POP3Addr        string
	POP3TLSAddr     string
	POP3Username    string
	POP3Password    string
	POP3Mailbox     string
	POP3UserRouting bool
	POP3Lock        bool

	// TLS
	TLSMode     TLSMode
	TLSCertFile string
	TLSKeyFile  string

	// Networking
	IdleTimeout time.Duration

	// Logging
	LogLevel  string
	LogFormat string
}

// Load reads the configuration from the process environment, applying the
// documented defaults and rejecting values that cannot work at runtime.
func Load() (*Config, error) {
	c := &Config{
		Hostname:   env("MS_HOSTNAME", "mailschleuse.local"),
		PublicHost: env("MS_PUBLIC_HOST", "localhost"),
		Mailboxes:  splitList(env("MS_MAILBOXES", "inbox,outbox")),

		LogLevel:  strings.ToLower(env("MS_LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(env("MS_LOG_FORMAT", "text")),

		DataDir: env("MS_DATA_DIR", ""),

		HTTPAddr:    env("MS_HTTP_ADDR", ":8080"),
		WebUsername: env("MS_WEB_USERNAME", ""),
		WebPassword: env("MS_WEB_PASSWORD", ""),
		BasePath:    normalizeBasePath(env("MS_WEB_BASE_PATH", "/")),
		CORSOrigin:  env("MS_WEB_CORS_ORIGIN", ""),

		SMTPAddr:     env("MS_SMTP_ADDR", ":1025"),
		SMTPTLSAddr:  env("MS_SMTP_TLS_ADDR", ""),
		SMTPUsername: env("MS_SMTP_USERNAME", ""),
		SMTPPassword: env("MS_SMTP_PASSWORD", ""),
		SMTPMailbox:  env("MS_SMTP_MAILBOX", "outbox"),

		POP3Addr:     env("MS_POP3_ADDR", ":1110"),
		POP3TLSAddr:  env("MS_POP3_TLS_ADDR", ""),
		POP3Username: env("MS_POP3_USERNAME", ""),
		POP3Password: env("MS_POP3_PASSWORD", ""),
		POP3Mailbox:  env("MS_POP3_MAILBOX", "inbox"),

		TLSCertFile: env("MS_TLS_CERT_FILE", ""),
		TLSKeyFile:  env("MS_TLS_KEY_FILE", ""),
	}

	var err error
	if c.MaxMessages, err = envInt("MS_MAX_MESSAGES", 500); err != nil {
		return nil, err
	}
	if c.MaxSizeBytes, err = envBytes("MS_MAX_MESSAGE_SIZE", 25*1024*1024); err != nil {
		return nil, err
	}
	if c.SMTPMaxRcpt, err = envInt("MS_SMTP_MAX_RECIPIENTS", 100); err != nil {
		return nil, err
	}
	if c.ReadOnlyMode, err = envBool("MS_WEB_READ_ONLY", false); err != nil {
		return nil, err
	}
	if c.SMTPUserRouting, err = envBool("MS_SMTP_MAILBOX_FROM_USERNAME", false); err != nil {
		return nil, err
	}
	if c.SMTPHeaderRoute, err = envBool("MS_SMTP_HEADER_ROUTING", true); err != nil {
		return nil, err
	}
	if c.SMTPAddReceived, err = envBool("MS_SMTP_ADD_RECEIVED", true); err != nil {
		return nil, err
	}
	if c.POP3UserRouting, err = envBool("MS_POP3_MAILBOX_FROM_USERNAME", false); err != nil {
		return nil, err
	}
	if c.POP3Lock, err = envBool("MS_POP3_EXCLUSIVE_LOCK", true); err != nil {
		return nil, err
	}
	if c.IdleTimeout, err = envDuration("MS_IDLE_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.SMTPAuth, err = authMode("MS_SMTP_AUTH", AuthOptional); err != nil {
		return nil, err
	}
	if c.TLSMode, err = tlsMode("MS_TLS_MODE", TLSAuto); err != nil {
		return nil, err
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	if len(c.Mailboxes) == 0 {
		return fmt.Errorf("MS_MAILBOXES must name at least one mailbox")
	}
	seen := map[string]bool{}
	for _, m := range c.Mailboxes {
		if !ValidMailboxName(m) {
			return fmt.Errorf("invalid mailbox name %q: use 1-32 chars of a-z, 0-9, '-' or '_'", m)
		}
		if seen[m] {
			return fmt.Errorf("mailbox %q is listed twice in MS_MAILBOXES", m)
		}
		seen[m] = true
	}
	if !seen[c.SMTPMailbox] {
		return fmt.Errorf("MS_SMTP_MAILBOX=%q is not part of MS_MAILBOXES", c.SMTPMailbox)
	}
	if !seen[c.POP3Mailbox] {
		return fmt.Errorf("MS_POP3_MAILBOX=%q is not part of MS_MAILBOXES", c.POP3Mailbox)
	}
	if c.SMTPAuth == AuthRequired && c.SMTPUsername == "" {
		return fmt.Errorf("MS_SMTP_AUTH=required needs MS_SMTP_USERNAME and MS_SMTP_PASSWORD")
	}
	if c.SMTPUsername != "" && c.SMTPPassword == "" {
		return fmt.Errorf("MS_SMTP_USERNAME is set but MS_SMTP_PASSWORD is empty")
	}
	if c.POP3Username != "" && c.POP3Password == "" {
		return fmt.Errorf("MS_POP3_USERNAME is set but MS_POP3_PASSWORD is empty")
	}
	if c.WebUsername != "" && c.WebPassword == "" {
		return fmt.Errorf("MS_WEB_USERNAME is set but MS_WEB_PASSWORD is empty")
	}
	if c.TLSMode == TLSFiles && (c.TLSCertFile == "" || c.TLSKeyFile == "") {
		return fmt.Errorf("MS_TLS_MODE=files needs MS_TLS_CERT_FILE and MS_TLS_KEY_FILE")
	}
	if c.TLSMode == TLSOff && (c.SMTPTLSAddr != "" || c.POP3TLSAddr != "") {
		return fmt.Errorf("implicit TLS listeners need MS_TLS_MODE=auto or files")
	}
	if c.MaxMessages < 1 {
		return fmt.Errorf("MS_MAX_MESSAGES must be >= 1")
	}
	if c.MaxSizeBytes < 1024 {
		return fmt.Errorf("MS_MAX_MESSAGE_SIZE must be at least 1024 bytes")
	}
	return nil
}

// HasMailbox reports whether name is a configured mailbox.
func (c *Config) HasMailbox(name string) bool {
	for _, m := range c.Mailboxes {
		if m == name {
			return true
		}
	}
	return false
}

// ValidMailboxName reports whether name is usable as a mailbox identifier. The
// character set is deliberately narrow: mailbox names show up in URLs, in POP3
// user names and (with persistence enabled) as directory names on disk.
func ValidMailboxName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := env(key, "")
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	return v, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := env(key, "")
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s: %q is not a boolean", key, raw)
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := env(key, "")
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. 30s, 5m)", key, raw)
	}
	return d, nil
}

// envBytes parses a byte size, accepting plain numbers as well as the common
// KB/MB/GB suffixes so MS_MAX_MESSAGE_SIZE=25MB reads naturally in a .env file.
func envBytes(key string, fallback int64) (int64, error) {
	raw := strings.ToUpper(env(key, ""))
	if raw == "" {
		return fallback, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(raw, "KB"), strings.HasSuffix(raw, "K"):
		mult, raw = 1024, strings.TrimSuffix(strings.TrimSuffix(raw, "KB"), "K")
	case strings.HasSuffix(raw, "MB"), strings.HasSuffix(raw, "M"):
		mult, raw = 1024*1024, strings.TrimSuffix(strings.TrimSuffix(raw, "MB"), "M")
	case strings.HasSuffix(raw, "GB"), strings.HasSuffix(raw, "G"):
		mult, raw = 1024*1024*1024, strings.TrimSuffix(strings.TrimSuffix(raw, "GB"), "G")
	case strings.HasSuffix(raw, "B"):
		raw = strings.TrimSuffix(raw, "B")
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a byte size (e.g. 25MB)", key, env(key, ""))
	}
	return v * mult, nil
}

func authMode(key string, fallback AuthMode) (AuthMode, error) {
	raw := strings.ToLower(env(key, string(fallback)))
	switch AuthMode(raw) {
	case AuthDisabled, AuthOptional, AuthRequired:
		return AuthMode(raw), nil
	}
	return "", fmt.Errorf("%s: %q must be one of disabled, optional, required", key, raw)
}

func tlsMode(key string, fallback TLSMode) (TLSMode, error) {
	raw := strings.ToLower(env(key, string(fallback)))
	switch TLSMode(raw) {
	case TLSOff, TLSAuto, TLSFiles:
		return TLSMode(raw), nil
	}
	return "", fmt.Errorf("%s: %q must be one of off, auto, files", key, raw)
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeBasePath turns a user supplied prefix into "/" or "/prefix".
func normalizeBasePath(raw string) string {
	raw = "/" + strings.Trim(raw, "/")
	if raw == "/" {
		return raw
	}
	return raw
}
