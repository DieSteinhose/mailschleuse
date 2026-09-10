package config

import "testing"

func TestRouteMatching(t *testing.T) {
	cases := []struct {
		pattern string
		address string
		want    bool
	}{
		{"helpdesk@example.com", "helpdesk@example.com", true},
		{"helpdesk@example.com", "HelpDesk@Example.COM", true},
		{"helpdesk@example.com", "other@example.com", false},
		{"*@example.com", "anyone@example.com", true},
		{"*@example.com", "anyone@other.com", false},
		{"@example.com", "anyone@example.com", true},
		{"support@*", "support@example.com", true},
		{"support@*", "sales@example.com", false},
		{"*", "whoever@wherever.test", true},
		{"*@example.com", "user@sub.example.com", false},
	}
	for _, c := range cases {
		route := Route{Pattern: c.pattern}
		if got := route.Matches(c.address); got != c.want {
			t.Errorf("Route(%q).Matches(%q) = %v, want %v", c.pattern, c.address, got, c.want)
		}
	}
}

func TestParseRoutes(t *testing.T) {
	known := func(name string) bool { return name == "inbox" || name == "outbox" }

	routes, err := ParseRoutes("helpdesk@example.com=inbox; *@example.com=inbox,outbox", known)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	if routes[0].Pattern != "helpdesk@example.com" || len(routes[0].Mailboxes) != 1 {
		t.Errorf("first route = %+v", routes[0])
	}
	if len(routes[1].Mailboxes) != 2 || routes[1].Mailboxes[1] != "outbox" {
		t.Errorf("second route = %+v", routes[1])
	}

	// Newlines work too, which keeps a longer list readable in a .env file.
	multiline, err := ParseRoutes("a@example.com=inbox\nb@example.com=outbox", known)
	if err != nil || len(multiline) != 2 {
		t.Fatalf("multiline parse = %v, %v", multiline, err)
	}

	if routes, err := ParseRoutes("", known); err != nil || routes != nil {
		t.Errorf("empty configuration should yield no routes, got %v, %v", routes, err)
	}
}

func TestParseRoutesRejectsBadInput(t *testing.T) {
	known := func(name string) bool { return name == "inbox" }
	for name, raw := range map[string]string{
		"no separator":     "helpdesk@example.com",
		"unknown mailbox":  "helpdesk@example.com=nowhere",
		"no mailbox named": "helpdesk@example.com=",
		"empty pattern":    "=inbox",
		"two wildcards":    "*@*=inbox",
	} {
		if _, err := ParseRoutes(raw, known); err == nil {
			t.Errorf("%s: %q was accepted", name, raw)
		}
	}
}

func TestRoutesFromEnvironment(t *testing.T) {
	t.Setenv("MS_SMTP_ROUTES", "helpdesk@example.com=inbox")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.SMTPRoutes) != 1 || !cfg.SMTPRoutes[0].Matches("helpdesk@example.com") {
		t.Fatalf("routes = %+v", cfg.SMTPRoutes)
	}

	t.Setenv("MS_SMTP_ROUTES", "helpdesk@example.com=does-not-exist")
	if _, err := Load(); err == nil {
		t.Error("a route naming an unknown mailbox should fail at startup")
	}
}
