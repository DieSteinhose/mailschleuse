package config

import (
	"fmt"
	"strings"
)

// Route delivers mail addressed to a matching recipient into specific
// mailboxes. It exists because a mail sink that keeps incoming and outgoing
// mail apart cannot, on its own, serve an application that mails itself: a
// self-test message is submitted over SMTP but has to be fetchable over POP3.
// A route makes that loop explicit and limited to the addresses you name.
type Route struct {
	// Pattern is an address ("a@b.tld"), a domain ("*@b.tld" or "@b.tld"),
	// a local part ("a@*") or the catch-all "*".
	Pattern string
	// Mailboxes receive a copy each, in the order given.
	Mailboxes []string
}

// Matches reports whether the route applies to a recipient address.
func (r Route) Matches(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	pattern := strings.ToLower(r.Pattern)

	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*@"):
		return strings.HasSuffix(address, pattern[1:])
	case strings.HasPrefix(pattern, "@"):
		return strings.HasSuffix(address, pattern)
	case strings.HasSuffix(pattern, "@*"):
		return strings.HasPrefix(address, pattern[:len(pattern)-1])
	default:
		return address == pattern
	}
}

// String renders the route the way it is written in the configuration.
func (r Route) String() string {
	return r.Pattern + "=" + strings.Join(r.Mailboxes, ",")
}

// ParseRoutes reads the MS_SMTP_ROUTES syntax: rules separated by ";" or a
// newline, each "pattern=mailbox" with a comma separated list of mailboxes.
//
//	helpdesk@example.com=inbox
//	*@example.com=inbox,outbox; support@*=inbox
func ParseRoutes(raw string, known func(string) bool) ([]Route, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var routes []Route
	for _, rule := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' }) {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		pattern, targets, found := strings.Cut(rule, "=")
		pattern = strings.TrimSpace(pattern)
		if !found || pattern == "" {
			return nil, fmt.Errorf("route %q: expected the form pattern=mailbox", rule)
		}
		if strings.Count(pattern, "*") > 1 {
			return nil, fmt.Errorf("route %q: use at most one wildcard", rule)
		}

		route := Route{Pattern: pattern}
		for _, target := range strings.Split(targets, ",") {
			target = strings.ToLower(strings.TrimSpace(target))
			if target == "" {
				continue
			}
			if !known(target) {
				return nil, fmt.Errorf("route %q: %q is not part of MS_MAILBOXES", rule, target)
			}
			route.Mailboxes = append(route.Mailboxes, target)
		}
		if len(route.Mailboxes) == 0 {
			return nil, fmt.Errorf("route %q: names no mailbox", rule)
		}
		routes = append(routes, route)
	}
	return routes, nil
}
