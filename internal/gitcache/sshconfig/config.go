// Package sshconfig resolves a deliberately bounded subset of OpenSSH client
// configuration without executing commands, opening agents, or dialing.
// A resolution with any error must never be used to establish a connection.
package sshconfig

import (
	"bufio"
	"errors"
	"fmt"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxFileBytes  = 64 * 1024
	maxTotalBytes = 256 * 1024
	maxFiles      = 64
	maxDepth      = 8
)

// Input specifies the destination and existing configuration sources, not
// duplicate SSH settings. Empty file paths select the normal OpenSSH paths.
// LookupEnv is injected to avoid reading unrelated process environment.
type Input struct {
	Host, LocalUser, Home    string
	RemoteUser               string // URL user; used when config has no User, including %r
	RemotePort               int    // URL port; used when config has no Port, including %p. Zero means 22.
	UserConfig, SystemConfig string
	LookupEnv                func(string) (string, bool)
}

// Config contains resolved settings. Paths and environment-derived values are
// private application data; callers must not dump this structure in diagnostics.
type Config struct {
	HashKnownHosts                             bool
	HostName, User                             string
	Port                                       int
	IdentityFiles                              []string
	IdentityAgent                              string // empty means no agent, including IdentityAgent none
	IdentitiesOnly                             bool
	UserKnownHostsFiles, GlobalKnownHostsFiles []string
	StrictHostKeyChecking                      string // yes, ask, accept-new, no; policy remains separate
	UpdateHostKeys                             string // yes, ask, no; policy remains separate
	PublicKeyAuthentication                    bool
	ControlPath                                string
	ControlPathDormant                         bool
	WarnWeakCrypto                             string
	SetEnv                                     []EnvironmentVariable
	SendEnv                                    []string
}

type configError struct {
	source            string
	line              int
	directive, reason string
}

func (e *configError) Error() string {
	return fmt.Sprintf("ssh config: %s line %d: %s: %s", e.source, e.line, e.directive, e.reason)
}
func problem(source string, line int, key, reason string) error {
	// Unknown option names may themselves contain attacker-controlled text.
	if !knownDirective(key) {
		key = "unknown directive"
	}
	return &configError{source, line, key, reason}
}
func knownDirective(k string) bool {
	switch k {
	case "host", "match", "include", "hostname", "user", "port", "identityfile", "identityagent", "identitiesonly", "userknownhostsfile", "globalknownhostsfile", "stricthostkeychecking", "updatehostkeys", "hashknownhosts", "pubkeyauthentication", "canonicalizehostname", "proxycommand", "proxyjump", "knownhostscommand", "localcommand", "permitlocalcommand", "controlmaster", "controlpath", "controlpersist", "setenv", "sendenv", "ignoreunknown", "warnweakcrypto", "addkeystoagent", "usekeychain", "certificatefile", "hostkeyalgorithms", "pubkeyacceptedalgorithms", "kexalgorithms", "ciphers", "macs", "forwardagent", "remotecommand":
		return true
	}
	return false
}

type value struct {
	args   []string
	source string
	line   int
}
type resolver struct {
	in               Input
	values           map[string]value
	identities       []value
	sendEnv          []string
	files, total     int
	stack            map[string]bool
	checkPermissions bool
}

// Resolve loads user configuration before system configuration. First scalar
// value wins; IdentityFile is cumulative. Unsupported applicable directives are
// errors, even if IgnoreUnknown names them. No security policy is overridden.
func Resolve(in Input) (Config, error) {
	if in.Host == "" || in.LocalUser == "" || !filepath.IsAbs(in.Home) {
		return Config{}, errors.New("ssh config: host, local user, and absolute home are required")
	}
	if strings.ContainsAny(in.Host, "\x00\r\n") {
		return Config{}, errors.New("ssh config: invalid target host")
	}
	if in.LookupEnv == nil {
		in.LookupEnv = os.LookupEnv
	}
	if in.UserConfig == "" {
		in.UserConfig = filepath.Join(in.Home, ".ssh", "config")
	}
	if in.SystemConfig == "" {
		in.SystemConfig = "/etc/ssh/ssh_config"
	}
	r := &resolver{in: in, values: map[string]value{}, stack: map[string]bool{}}
	// Each top-level file starts with its own active global section.
	for _, src := range []struct{ path, label, base string }{{in.UserConfig, "user configuration", filepath.Join(in.Home, ".ssh")}, {in.SystemConfig, "system configuration", "/etc/ssh"}} {
		active := true
		r.checkPermissions = src.label == "user configuration"
		if err := r.read(src.path, src.label, src.base, &active, 0, true); err != nil {
			return Config{}, err
		}
	}
	return r.finish()
}

func (r *resolver) read(path, label, base string, active *bool, depth int, optional bool) error {
	if depth > maxDepth || r.files >= maxFiles {
		return problem(label, 0, "include", "configuration inclusion limit exceeded")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return problem(label, 0, "include", "configuration path unavailable")
	}
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) && optional {
		return nil
	}
	if err != nil {
		return problem(label, 0, "include", "configuration file unavailable")
	}
	if r.stack[real] {
		return problem(label, 0, "include", "configuration inclusion cycle")
	}
	f, st, err := sshfile.OpenRegular(real, maxFileBytes)
	if err != nil {
		return problem(label, 0, "include", "configuration file unavailable or exceeds size limit")
	}
	defer f.Close()
	if (r.checkPermissions || depth > 0) && !sshfile.UserConfigOK(st) {
		return problem(label, 0, "include", "unsafe configuration owner or permissions")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes || r.total+len(data) > maxTotalBytes {
		return problem(label, 0, "include", "configuration read or size limit failure")
	}
	r.files++
	r.total += len(data)
	r.stack[real] = true
	defer delete(r.stack, real)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 1024), maxFileBytes+1)
	line := 0
	for scanner.Scan() {
		line++
		args, err := splitLine(scanner.Text())
		if err != nil {
			return problem(label, line, "unknown", "invalid quoting or syntax")
		}
		if len(args) == 0 {
			continue
		}
		key := strings.ToLower(args[0])
		args = args[1:]
		if len(args) == 0 {
			return problem(label, line, key, "missing argument")
		}
		switch key {
		case "host":
			*active = matches(r.in.Host, args, false)
			continue
		case "match":
			yes, err := r.match(args, label, line)
			if err != nil {
				return err
			}
			*active = yes
			continue
		}
		if !*active {
			continue
		}
		if key == "include" {
			for _, raw := range args {
				// Includes need only home/env expansion; host/user-dependent tokens could
				// depend on a later setting and are rejected rather than guessed.
				expanded, err := r.expand(raw, "include", label, line, "", "", 0)
				if err != nil {
					return err
				}
				if !filepath.IsAbs(expanded) {
					expanded = filepath.Join(base, expanded)
				}
				paths, err := filepath.Glob(expanded)
				if err != nil {
					return problem(label, line, key, "invalid include pattern")
				}
				if len(paths) > maxFiles-r.files {
					return problem(label, line, key, "configuration inclusion limit exceeded")
				}
				for _, p := range paths {
					childActive := *active
					if err := r.read(p, "included configuration", base, &childActive, depth+1, false); err != nil {
						return err
					}
				}
			}
			continue
		}
		if key == "sendenv" {
			for _, pattern := range args {
				if strings.HasPrefix(pattern, "-") {
					kept := r.sendEnv[:0]
					for _, previous := range r.sendEnv {
						if !wildcard(previous, pattern[1:]) {
							kept = append(kept, previous)
						}
					}
					r.sendEnv = kept
				} else {
					r.sendEnv = append(r.sendEnv, pattern)
				}
			}
			continue
		}
		if key == "identityfile" {
			if len(args) != 1 {
				return problem(label, line, key, "expected one path")
			}
			r.identities = append(r.identities, value{args, label, line})
			continue
		}
		switch key {
		case "hostname", "user", "port", "identityagent", "identitiesonly", "stricthostkeychecking", "updatehostkeys", "hashknownhosts", "pubkeyauthentication", "canonicalizehostname", "controlmaster", "controlpath", "controlpersist", "warnweakcrypto", "ignoreunknown":
			if len(args) != 1 {
				return problem(label, line, key, "expected one argument")
			}
		case "userknownhostsfile", "globalknownhostsfile", "setenv":
		default:
			return problem(label, line, key, "unsupported applicable setting; native SSH will not ignore it or execute helpers")
		}
		if _, ok := r.values[key]; !ok {
			r.values[key] = value{args, label, line}
		}
	}
	if scanner.Err() != nil {
		return problem(label, line, "unknown", "configuration scan failed")
	}
	return nil
}

func (r *resolver) match(args []string, src string, line int) (bool, error) {
	if len(args) == 1 && strings.EqualFold(args[0], "all") {
		return true, nil
	}
	yes, unsupported := true, false
	for i := 0; i < len(args); {
		criterion := strings.ToLower(args[i])
		i++
		neg := strings.HasPrefix(criterion, "!")
		criterion = strings.TrimPrefix(criterion, "!")
		if criterion == "canonical" || criterion == "final" {
			unsupported = true
			continue
		}
		if criterion == "all" {
			return false, problem(src, line, "match", "unsupported or invalid combined all criterion")
		}
		if i >= len(args) {
			return false, problem(src, line, "match", "criterion requires an argument")
		}
		pattern := args[i]
		i++
		var subject string
		switch criterion {
		case "host":
			subject = r.in.Host
			if v, ok := r.values["hostname"]; ok {
				var err error
				subject, err = r.expand(v.args[0], "hostname", v.source, v.line, r.in.Host, r.in.LocalUser, 22)
				if err != nil {
					return false, err
				}
			}
		case "originalhost":
			subject = r.in.Host
		case "user":
			subject = r.remoteUser()
			if v, ok := r.values["user"]; ok {
				subject = v.args[0]
			}
		case "localuser":
			subject = r.in.LocalUser
		case "exec", "localnetwork", "tagged", "command", "version", "sessiontype":
			unsupported = true
			continue
		default:
			return false, problem(src, line, "match", "unrecognized criterion")
		}
		hit := matches(subject, strings.Split(pattern, ","), criterion == "user" || criterion == "localuser")
		if neg {
			hit = !hit
		}
		yes = yes && hit
	}
	// A supported false conjunct proves this block cannot affect the target.
	// No unsupported predicate (especially exec) is ever evaluated.
	if !yes {
		return false, nil
	}
	if unsupported {
		return false, problem(src, line, "match", "unsupported applicable criterion; exec and canonical/final passes are not executed")
	}
	return true, nil
}

// SSH wildcards are only '*' and '?', not filepath glob character classes.
func wildcard(s, p string) bool {
	si, pi, star, retry := 0, 0, -1, 0
	for si < len(s) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == s[si]) {
			si++
			pi++
			continue
		}
		if pi < len(p) && p[pi] == '*' {
			star = pi
			pi++
			retry = si
			continue
		}
		if star >= 0 {
			retry++
			si = retry
			pi = star + 1
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
func matches(s string, patterns []string, caseSensitive bool) bool {
	if !caseSensitive {
		s = strings.ToLower(s)
	}
	positive := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if !caseSensitive {
			p = strings.ToLower(p)
		}
		if wildcard(s, p) {
			if neg {
				return false
			}
			positive = true
		}
	}
	return positive
}

func (r *resolver) remoteUser() string {
	if r.in.RemoteUser != "" {
		return r.in.RemoteUser
	}
	return r.in.LocalUser
}

func (r *resolver) scalar(key, def string) string {
	if v, ok := r.values[key]; ok {
		return v.args[0]
	}
	return def
}
func (r *resolver) bad(key, reason string) error {
	v := r.values[key]
	return problem(v.source, v.line, key, reason)
}
func (r *resolver) finish() (Config, error) {
	port := 22
	if r.in.RemotePort != 0 {
		if r.in.RemotePort < 1 || r.in.RemotePort > 65535 {
			return Config{}, errors.New("ssh config: invalid remote port")
		}
		port = r.in.RemotePort
	}
	c := Config{HostName: r.in.Host, User: r.scalar("user", r.remoteUser()), Port: port, StrictHostKeyChecking: r.scalar("stricthostkeychecking", "ask"), UpdateHostKeys: r.scalar("updatehostkeys", "yes"), PublicKeyAuthentication: true}
	if r.scalar("canonicalizehostname", "no") != "no" {
		return Config{}, r.bad("canonicalizehostname", "canonicalization is unsupported")
	}
	if v, ok := r.values["hostname"]; ok {
		host, err := r.expand(v.args[0], "hostname", v.source, v.line, r.in.Host, c.User, c.Port)
		if err != nil {
			return Config{}, err
		}
		c.HostName = host
	}
	if port, ok := r.values["port"]; ok {
		n, err := strconv.Atoi(port.args[0])
		if err != nil || n < 1 || n > 65535 {
			return Config{}, r.bad("port", "invalid port")
		}
		c.Port = n
	}
	if strings.Contains(c.User, "%") || strings.Contains(c.User, "${") {
		return Config{}, r.bad("user", "User token/environment expansion is unsupported")
	}
	if c.HostName == "" || c.User == "" || strings.ContainsAny(c.HostName+c.User, "\x00\r\n") {
		return Config{}, errors.New("ssh config: invalid resolved host or user")
	}
	for key, dst := range map[string]*bool{"identitiesonly": &c.IdentitiesOnly, "pubkeyauthentication": &c.PublicKeyAuthentication, "hashknownhosts": &c.HashKnownHosts} {
		if v, ok := r.values[key]; ok {
			switch v.args[0] {
			case "yes":
				*dst = true
			case "no":
				*dst = false
			default:
				return Config{}, r.bad(key, "expected yes or no")
			}
		}
	}
	switch c.StrictHostKeyChecking {
	case "yes", "ask", "accept-new", "no", "off":
		if c.StrictHostKeyChecking == "off" {
			c.StrictHostKeyChecking = "no"
		}
	default:
		return Config{}, r.bad("stricthostkeychecking", "unsupported value")
	}
	switch c.UpdateHostKeys {
	case "yes", "ask", "no":
	default:
		return Config{}, r.bad("updatehostkeys", "unsupported value")
	}
	// OpenSSH disables the automatic update default with custom user known_hosts
	// or DNS host verification. DNS settings are currently rejected as unsupported.
	if _, set := r.values["updatehostkeys"]; !set {
		if _, custom := r.values["userknownhostsfile"]; custom {
			c.UpdateHostKeys = "no"
		}
	}
	ids := r.identities
	if len(ids) == 0 {
		for _, name := range []string{"id_rsa", "id_ecdsa", "id_ecdsa_sk", "id_ed25519", "id_ed25519_sk"} {
			ids = append(ids, value{[]string{"~/.ssh/" + name}, "defaults", 0})
		}
	}
	for _, v := range ids {
		if v.args[0] == "none" {
			continue
		}
		p, err := r.expand(v.args[0], "identityfile", v.source, v.line, c.HostName, c.User, c.Port)
		if err != nil {
			return Config{}, err
		}
		if !contains(c.IdentityFiles, p) {
			c.IdentityFiles = append(c.IdentityFiles, p)
		}
	}
	for _, spec := range []struct {
		key  string
		defs []string
		out  *[]string
	}{{"userknownhostsfile", []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2"}, &c.UserKnownHostsFiles}, {"globalknownhostsfile", []string{"/etc/ssh/ssh_known_hosts", "/etc/ssh/ssh_known_hosts2"}, &c.GlobalKnownHostsFiles}} {
		v, ok := r.values[spec.key]
		if !ok {
			v = value{spec.defs, "defaults", 0}
		}
		if len(v.args) == 1 && v.args[0] == "none" {
			continue
		}
		for _, raw := range v.args {
			if raw == "none" {
				return Config{}, r.bad(spec.key, "none must appear alone")
			}
			p, err := r.expand(raw, spec.key, v.source, v.line, c.HostName, c.User, c.Port)
			if err != nil {
				return Config{}, err
			}
			*spec.out = append(*spec.out, p)
		}
	}
	v, hasAgent := r.values["identityagent"]
	if !hasAgent {
		c.IdentityAgent, _ = r.in.LookupEnv("SSH_AUTH_SOCK")
	} else if v.args[0] != "none" {
		raw := v.args[0]
		if raw == "SSH_AUTH_SOCK" {
			c.IdentityAgent, _ = r.in.LookupEnv("SSH_AUTH_SOCK")
		} else if strings.HasPrefix(raw, "$") && !strings.HasPrefix(raw, "${") {
			name := strings.TrimPrefix(raw, "$")
			if !envName(name) {
				return Config{}, r.bad("identityagent", "invalid environment reference")
			}
			var ok bool
			c.IdentityAgent, ok = r.in.LookupEnv(name)
			if !ok {
				return Config{}, r.bad("identityagent", "referenced environment variable is unset")
			}
		} else {
			var err error
			c.IdentityAgent, err = r.expand(raw, "identityagent", v.source, v.line, c.HostName, c.User, c.Port)
			if err != nil {
				return Config{}, err
			}
		}
	}

	if err := r.sessionSettings(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
func envName(s string) bool {
	if s == "" {
		return false
	}
	for i, b := range []byte(s) {
		if !(b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || i > 0 && b >= '0' && b <= '9') {
			return false
		}
	}
	return true
}

func (r *resolver) expand(raw, key, src string, line int, host, user string, port int) (string, error) {
	if (key == "globalknownhostsfile" || key == "hostname") && strings.Contains(raw, "${") {
		return "", problem(src, line, key, "environment expansion is unsupported for this directive")
	}
	if key == "globalknownhostsfile" && strings.Contains(raw, "%") {
		return "", problem(src, line, key, "token expansion is unsupported for this directive")
	}
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' {
			i++
			if i >= len(raw) {
				return "", problem(src, line, key, "incomplete token")
			}
			token := raw[i]
			replacement := ""
			switch token {
			case '%':
				replacement = "%"
			case 'd':
				replacement = r.in.Home
			case 'h':
				replacement = host
			case 'n':
				replacement = r.in.Host
			case 'r':
				replacement = user
			case 'u':
				replacement = r.in.LocalUser
			case 'p':
				if port != 0 {
					replacement = strconv.Itoa(port)
				}
			default:
				return "", problem(src, line, key, "unsupported token")
			}
			if key == "hostname" && token != '%' && token != 'h' {
				return "", problem(src, line, key, "unsupported HostName token")
			}
			if key == "include" && token != '%' && token != 'd' && token != 'u' {
				return "", problem(src, line, key, "host-dependent Include tokens are unsupported")
			}
			out.WriteString(replacement)
		} else if raw[i] == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			end := strings.IndexByte(raw[i+2:], '}')
			if end < 0 {
				return "", problem(src, line, key, "incomplete environment reference")
			}
			end += i + 2
			name := raw[i+2 : end]
			if !envName(name) {
				return "", problem(src, line, key, "invalid environment reference")
			}
			val, ok := r.in.LookupEnv(name)
			if !ok {
				return "", problem(src, line, key, "referenced environment variable is unset")
			}
			out.WriteString(val)
			i = end
		} else {
			out.WriteByte(raw[i])
		}
	}
	result := out.String()
	if key != "setenv" && key != "hostname" && result == "~" {
		result = r.in.Home
	} else if key != "setenv" && key != "hostname" && strings.HasPrefix(result, "~/") {
		result = filepath.Join(r.in.Home, result[2:])
	} else if key != "setenv" && key != "hostname" && strings.HasPrefix(result, "~") {
		return "", problem(src, line, key, "other-user home expansion is unsupported")
	}
	if result == "" || strings.ContainsAny(result, "\x00\r\n") {
		return "", problem(src, line, key, "invalid expanded value")
	}
	return result, nil
}

// splitLine implements double-quoted arguments, escaped delimiters, comments,
// and the optional single '=' between directive and argument. It is not a shell.
func splitLine(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return nil, nil
	}
	end := 0
	for end < len(line) && line[end] != '=' && line[end] != ' ' && line[end] != '\t' {
		end++
	}
	words := []string{line[:end]}
	line = strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(line, "=") {
		line = strings.TrimLeft(line[1:], " \t")
	}
	if strings.HasPrefix(line, "=") {
		return nil, errors.New("equals")
	}
	var b strings.Builder
	quote, started := false, false
	flush := func() {
		if started {
			words = append(words, b.String())
			b.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' {
			if i+1 == len(line) {
				return nil, errors.New("escape")
			}
			next := line[i+1]
			if next == '"' || next == '\\' || next == '#' || next == ' ' || next == '\t' {
				b.WriteByte(next)
				started = true
				i++
				continue
			}
			b.WriteByte(c)
			started = true
			continue
		}
		if c == '"' {
			quote = !quote
			started = true
			continue
		}
		if !quote && c == '#' {
			break
		}
		if !quote && (c == ' ' || c == '\t' || c == '\r') {
			flush()
			continue
		}
		b.WriteByte(c)
		started = true
	}
	if quote {
		return nil, errors.New("quote")
	}
	flush()
	return words, nil
}
