package sshx

import (
	"strings"
	"testing"
)

func newTestAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	al, err := NewAllowlist(DefaultAllowlist())
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	return al
}

func TestRenderValidCommands(t *testing.T) {
	al := newTestAllowlist(t)
	cases := []struct {
		id     string
		params map[string]string
		want   string
	}{
		{"uptime", nil, "uptime"},
		{"free", nil, "free -h"},
		{"systemctl_status", map[string]string{"service": "nginx"}, "systemctl status nginx"},
		{"journalctl_service", map[string]string{"service": "apache2", "lines": "100"}, "journalctl -u apache2 -n 100 --no-pager"},
		{"lsof_port", map[string]string{"port": "8080"}, "lsof -i :8080"},
	}
	for _, c := range cases {
		got, err := al.Render(c.id, c.params)
		if err != nil {
			t.Errorf("Render(%q) error: %v", c.id, err)
			continue
		}
		if got != c.want {
			t.Errorf("Render(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestRenderRejects(t *testing.T) {
	al := newTestAllowlist(t)
	bad := []struct {
		id     string
		params map[string]string
	}{
		{"not_a_command", nil},
		{"systemctl_status", nil}, // missing param
		{"systemctl_status", map[string]string{"service": "nginx", "extra": "x"}}, // unexpected param
		{"systemctl_status", map[string]string{"service": "nginx; rm -rf /"}},
		{"systemctl_status", map[string]string{"service": "ng$(id)"}},
		{"systemctl_status", map[string]string{"service": "a|b"}},
		{"systemctl_status", map[string]string{"service": "a`id`"}},
		{"lsof_port", map[string]string{"port": "80;reboot"}},
	}
	for _, c := range bad {
		if _, err := al.Render(c.id, c.params); err == nil {
			t.Errorf("Render(%q, %v) should have been rejected", c.id, c.params)
		}
	}
}

func TestRenderedNeverContainsMetachars(t *testing.T) {
	al := newTestAllowlist(t)
	metachars := ";&|`$()<>*?[]{}~!#\\"
	for _, c := range al.List() {
		params := map[string]string{}
		for name, pattern := range c.Params {
			// pick a valid value per pattern where possible
			params[name] = sampleForPattern(pattern)
		}
		got, err := al.Render(c.ID, params)
		if err != nil {
			continue // not all patterns have trivially sampleable values
		}
		if strings.ContainsAny(got, metachars) {
			t.Errorf("command %q rendered %q containing metacharacter", c.ID, got)
		}
	}
}

func sampleForPattern(p string) string {
	switch p {
	case `\d{1,5}`, `\d{1,4}`, `\d{1,2}`:
		return "10"
	case `[A-Za-z0-9._\-]+`:
		return "svc-1"
	}
	return "x"
}

func TestRejectMetacharsInTemplate(t *testing.T) {
	_, err := NewAllowlist([]Command{{ID: "evil", Template: "uptime; rm -rf /"}})
	if err == nil {
		t.Fatal("expected error for metacharacter in template")
	}
}

func TestDuplicateIDs(t *testing.T) {
	_, err := NewAllowlist([]Command{
		{ID: "a", Template: "uptime"},
		{ID: "a", Template: "free"},
	})
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}
