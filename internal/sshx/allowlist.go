package sshx

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Command is a single allowlisted, read-only command template.
type Command struct {
	ID          string
	Description string
	Template    string
	Params      map[string]string // param name -> regex pattern the value must match
}

// Allowlist is the set of commands that may be executed. It is the only
// execution path exposed to the agent and to the API, enforcing read-only
// access. No command in the allowlist may contain shell metacharacters.
type Allowlist struct {
	commands map[string]*Command
	order    []string
}

// shellMetachars are rejected anywhere in a rendered command. This is
// defense-in-depth: templates come from trusted config, but params are
// agent/user supplied and must never enable command injection or a pipe to a
// write-capable binary.
const shellMetachars = ";&|`$\n\t\r()<>*?[]{}~!#\\"

// templateMetachars are rejected in command templates. Braces are excluded
// because they denote {placeholder} params; any leftover brace in a rendered
// command is caught by the shellMetachars check in Render.
const templateMetachars = ";&|`$\n\t\r()<>*?[]~!#\\"

var allowedParam = regexp.MustCompile(`^[A-Za-z0-9._:/\-]+$`)

func NewAllowlist(cmds []Command) (*Allowlist, error) {
	al := &Allowlist{commands: map[string]*Command{}}
	for i := range cmds {
		c := &cmds[i]
		if c.ID == "" || c.Template == "" {
			return nil, fmt.Errorf("command %q: id and template are required", c.ID)
		}
		if strings.ContainsAny(c.Template, templateMetachars) {
			return nil, fmt.Errorf("command %q: template contains shell metacharacters", c.ID)
		}
		for _, p := range c.Params {
			if _, err := regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("command %q param regex: %w", c.ID, err)
			}
		}
		if _, dup := al.commands[c.ID]; dup {
			return nil, fmt.Errorf("duplicate command id %q", c.ID)
		}
		al.commands[c.ID] = c
		al.order = append(al.order, c.ID)
	}
	return al, nil
}

// DefaultAllowlist returns the built-in read-only diagnostic command set.
func DefaultAllowlist() []Command {
	return []Command{
		{ID: "uptime", Description: "System uptime and load average", Template: "uptime"},
		{ID: "uname", Description: "Kernel/OS version", Template: "uname -a"},
		{ID: "os_release", Description: "Operating system release info", Template: "cat /etc/os-release"},
		{ID: "free", Description: "Memory usage", Template: "free -h"},
		{ID: "df", Description: "Filesystem disk usage", Template: "df -h"},
		{ID: "ps", Description: "Running processes", Template: "ps aux"},
		{ID: "top_snapshot", Description: "Snapshot of top processes", Template: "top -bn1"},
		{ID: "ss_listening", Description: "Listening TCP/UDP sockets", Template: "ss -tulpn"},
		{ID: "lsof_port", Description: "Processes on a port", Template: "lsof -i :{port}", Params: map[string]string{"port": `\d{1,5}`}},
		{ID: "systemctl_status", Description: "Status of a systemd service", Template: "systemctl status {service}", Params: map[string]string{"service": `[A-Za-z0-9._\-]+`}},
		{ID: "journalctl_service", Description: "Recent log lines of a systemd service", Template: "journalctl -u {service} -n {lines} --no-pager", Params: map[string]string{"service": `[A-Za-z0-9._\-]+`, "lines": `\d{1,4}`}},
		{ID: "journalctl_boot", Description: "Logs from current boot", Template: "journalctl -b -n {lines} --no-pager", Params: map[string]string{"lines": `\d{1,4}`}},
		{ID: "loadavg", Description: "Load average from /proc", Template: "cat /proc/loadavg"},
		{ID: "meminfo", Description: "Kernel memory info", Template: "cat /proc/meminfo"},
		{ID: "disk_inodes", Description: "Filesystem inode usage", Template: "df -i"},
		{ID: "network_interfaces", Description: "Network interface config", Template: "ip -brief addr"},
		{ID: "systemctl_failed", Description: "Failed systemd units", Template: "systemctl --failed --no-pager"},
		{ID: "list_var_log", Description: "Recent files in /var/log", Template: "ls -la /var/log"},
		{ID: "dmesg_tail", Description: "Kernel ring buffer", Template: "dmesg -T"},
		{ID: "ping_host", Description: "ICMP reachability check", Template: "ping -c {count} -W 2 {host}", Params: map[string]string{"count": `\d{1,2}`, "host": `[A-Za-z0-9._\-]+`}},
		{ID: "hostname", Description: "Hostname and FQDN", Template: "hostname -f"},
		{ID: "date", Description: "Current system time", Template: "date"},
	}
}

// List returns command definitions in stable order.
func (al *Allowlist) List() []Command {
	out := make([]Command, 0, len(al.order))
	for _, id := range al.order {
		out = append(out, *al.commands[id])
	}
	return out
}

// Get returns a command definition by id.
func (al *Allowlist) Get(id string) (*Command, bool) {
	c, ok := al.commands[id]
	return c, ok
}

// Has reports whether a command id is allowed.
func (al *Allowlist) Has(id string) bool {
	_, ok := al.commands[id]
	return ok
}

// Render validates params for the given command id and renders the final
// shell-safe command string. It returns an error if the command is not
// allowlisted, if a required param is missing, if a param fails its regex,
// if unexpected params are supplied, or if the rendered command contains any
// shell metacharacter.
func (al *Allowlist) Render(id string, params map[string]string) (string, error) {
	c, ok := al.commands[id]
	if !ok {
		return "", fmt.Errorf("command %q is not in the allowlist", id)
	}
	if params == nil {
		params = map[string]string{}
	}
	for name := range params {
		if _, ok := c.Params[name]; !ok {
			return "", fmt.Errorf("command %q: unexpected parameter %q", id, name)
		}
	}
	for name, pattern := range c.Params {
		val, ok := params[name]
		if !ok || val == "" {
			return "", fmt.Errorf("command %q: missing required parameter %q", id, name)
		}
		if !allowedParam.MatchString(val) {
			return "", fmt.Errorf("command %q: parameter %q contains invalid characters", id, name)
		}
		if re, err := regexp.Compile(pattern); err == nil && !re.MatchString(val) {
			return "", fmt.Errorf("command %q: parameter %q does not match allowed pattern", id, name)
		}
	}
	rendered := c.Template
	for name, val := range params {
		rendered = strings.ReplaceAll(rendered, "{"+name+"}", val)
	}
	if strings.Contains(rendered, "{") || strings.Contains(rendered, "}") {
		return "", fmt.Errorf("command %q: unresolved placeholder in template", id)
	}
	if strings.ContainsAny(rendered, shellMetachars) {
		return "", fmt.Errorf("command %q: rendered command contains shell metacharacters", id)
	}
	return rendered, nil
}

// SortedIDs returns allowlisted command ids sorted alphabetically.
func (al *Allowlist) SortedIDs() []string {
	ids := append([]string{}, al.order...)
	sort.Strings(ids)
	return ids
}
