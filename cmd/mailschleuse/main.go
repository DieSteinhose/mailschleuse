// Command mailschleuse runs a self-contained mail sink for development and test
// environments: an SMTP endpoint that captures everything an application sends,
// a POP3 endpoint that serves mail an application is supposed to receive, and
// one web interface that covers both.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/diesteinhose/mailschleuse/internal/config"
	"github.com/diesteinhose/mailschleuse/internal/pop3d"
	"github.com/diesteinhose/mailschleuse/internal/smtpd"
	"github.com/diesteinhose/mailschleuse/internal/store"
	"github.com/diesteinhose/mailschleuse/internal/tlsutil"
	"github.com/diesteinhose/mailschleuse/internal/web"
)

// version is overwritten at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local HTTP endpoint and exit (used by the container healthcheck)")
	flag.Parse()

	if *showVersion {
		fmt.Println("mailschleuse", version)
		return
	}

	if *healthcheck {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("mailschleuse stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	messages, err := store.New(store.Options{
		Mailboxes:   cfg.Mailboxes,
		MaxMessages: cfg.MaxMessages,
		MaxSize:     cfg.MaxSizeBytes,
		DataDir:     cfg.DataDir,
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	tlsConfig, err := buildTLS(cfg, logger)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}

	smtpServer := smtpd.New(smtpd.Options{
		Hostname:       cfg.Hostname,
		Auth:           cfg.SMTPAuth,
		Username:       cfg.SMTPUsername,
		Password:       cfg.SMTPPassword,
		DefaultMailbox: cfg.SMTPMailbox,
		UserRouting:    cfg.SMTPUserRouting,
		HeaderRouting:  cfg.SMTPHeaderRoute,
		AddReceived:    cfg.SMTPAddReceived,
		MaxSize:        cfg.MaxSizeBytes,
		MaxRecipients:  cfg.SMTPMaxRcpt,
		IdleTimeout:    cfg.IdleTimeout,
		TLSConfig:      tlsConfig,
		Store:          messages,
		Logger:         logger,
	})

	pop3Server := pop3d.New(pop3d.Options{
		Hostname:       cfg.Hostname,
		Username:       cfg.POP3Username,
		Password:       cfg.POP3Password,
		DefaultMailbox: cfg.POP3Mailbox,
		UserRouting:    cfg.POP3UserRouting,
		ExclusiveLock:  cfg.POP3Lock,
		IdleTimeout:    cfg.IdleTimeout,
		TLSConfig:      tlsConfig,
		Store:          messages,
		Logger:         logger,
	})

	httpServer := &http.Server{
		Handler: web.NewHandler(web.Options{
			Store:      messages,
			Version:    version,
			Hostname:   cfg.Hostname,
			BasePath:   cfg.BasePath,
			Username:   cfg.WebUsername,
			Password:   cfg.WebPassword,
			CORSOrigin: cfg.CORSOrigin,
			ReadOnly:   cfg.ReadOnlyMode,
			MaxSize:    cfg.MaxSizeBytes,
			Endpoints:  describeEndpoints(cfg),
			Logger:     logger,
		}),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	// Bind every listener before serving so a port clash fails the start
	// instead of leaving the service half up.
	type listenerSpec struct {
		name        string
		addr        string
		implicitTLS bool
		serve       func(net.Listener, bool) error
	}
	specs := []listenerSpec{
		{"http", cfg.HTTPAddr, false, func(ln net.Listener, _ bool) error { return httpServer.Serve(ln) }},
		{"smtp", cfg.SMTPAddr, false, smtpServer.Serve},
		{"smtps", cfg.SMTPTLSAddr, true, smtpServer.Serve},
		{"pop3", cfg.POP3Addr, false, pop3Server.Serve},
		{"pop3s", cfg.POP3TLSAddr, true, pop3Server.Serve},
	}

	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		runErr  error
	)
	stopped := make(chan struct{})

	opened := make([]net.Listener, 0, len(specs))
	closeAll := func() {
		for _, ln := range opened {
			ln.Close()
		}
	}
	for _, spec := range specs {
		if strings.TrimSpace(spec.addr) == "" {
			continue
		}
		ln, err := net.Listen("tcp", spec.addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("listen %s on %s: %w", spec.name, spec.addr, err)
		}
		opened = append(opened, ln)
		logger.Info("listener ready", "protocol", spec.name, "address", ln.Addr().String())

		wg.Add(1)
		go func(spec listenerSpec, ln net.Listener) {
			defer wg.Done()
			err := spec.serve(ln, spec.implicitTLS)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errOnce.Do(func() { runErr = fmt.Errorf("%s: %w", spec.name, err) })
				select {
				case <-stopped:
				default:
					close(stopped)
				}
			}
		}(spec, ln)
	}

	logger.Info("mailschleuse started",
		"version", version, "mailboxes", strings.Join(cfg.Mailboxes, ","),
		"smtp_mailbox", cfg.SMTPMailbox, "pop3_mailbox", cfg.POP3Mailbox,
		"persistence", cfg.DataDir != "")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		logger.Info("shutting down", "signal", sig.String())
	case <-stopped:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdownCtx)
	smtpServer.Close()
	pop3Server.Close()
	wg.Wait()
	return runErr
}

// probe implements the container healthcheck so the image needs no shell or
// extra tooling: the binary checks its own HTTP endpoint.
func probe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http address %q: %w", cfg.HTTPAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := strings.TrimSuffix(cfg.BasePath, "/")
	url := fmt.Sprintf("http://%s/%s", net.JoinHostPort(host, port), strings.TrimPrefix(base+"/healthz", "/"))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// buildTLS resolves the certificate for STARTTLS/STLS and the implicit TLS
// listeners. A generated certificate is self-signed, so clients have to skip
// verification - which is the normal setup for a local test mail server.
func buildTLS(cfg *config.Config, logger *slog.Logger) (*tls.Config, error) {
	switch cfg.TLSMode {
	case config.TLSOff:
		return nil, nil
	case config.TLSFiles:
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		logger.Info("tls enabled", "mode", "files", "certificate", cfg.TLSCertFile)
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	default:
		cert, err := tlsutil.SelfSigned(cfg.Hostname, cfg.PublicHost)
		if err != nil {
			return nil, err
		}
		logger.Info("tls enabled", "mode", "auto",
			"note", "self-signed certificate, clients must skip verification")
		return &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}, nil
	}
}

// describeEndpoints builds the connection cheat sheet the UI shows, so the
// values to paste into an application's mail settings are never a guess.
func describeEndpoints(cfg *config.Config) []web.Endpoint {
	encryption := func(implicit bool) string {
		if cfg.TLSMode == config.TLSOff {
			return "none"
		}
		if implicit {
			return "implicit TLS (self-signed)"
		}
		return "none or STARTTLS (self-signed)"
	}

	endpoints := []web.Endpoint{{
		Protocol:     "SMTP",
		Host:         cfg.PublicHost,
		Port:         portOf(cfg.SMTPAddr),
		Mailbox:      cfg.SMTPMailbox,
		Username:     cfg.SMTPUsername,
		AuthRequired: cfg.SMTPAuth == config.AuthRequired,
		TLS:          encryption(false),
		Note:         "outgoing mail of your application lands here",
	}, {
		Protocol:     "POP3",
		Host:         cfg.PublicHost,
		Port:         portOf(cfg.POP3Addr),
		Mailbox:      cfg.POP3Mailbox,
		Username:     cfg.POP3Username,
		AuthRequired: cfg.POP3Username != "",
		TLS:          encryption(false),
		Note:         "your application fetches mail from here",
	}}

	if cfg.SMTPTLSAddr != "" {
		endpoints = append(endpoints, web.Endpoint{
			Protocol: "SMTPS", Host: cfg.PublicHost, Port: portOf(cfg.SMTPTLSAddr),
			Mailbox: cfg.SMTPMailbox, Username: cfg.SMTPUsername,
			AuthRequired: cfg.SMTPAuth == config.AuthRequired, TLS: encryption(true),
			Note: "same as SMTP, but TLS from the first byte",
		})
	}
	if cfg.POP3TLSAddr != "" {
		endpoints = append(endpoints, web.Endpoint{
			Protocol: "POP3S", Host: cfg.PublicHost, Port: portOf(cfg.POP3TLSAddr),
			Mailbox: cfg.POP3Mailbox, Username: cfg.POP3Username,
			AuthRequired: cfg.POP3Username != "", TLS: encryption(true),
			Note: "same as POP3, but TLS from the first byte",
		})
	}
	return endpoints
}

// portOf extracts the port from a listen address such as ":1025".
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return strings.TrimPrefix(addr, ":")
}
