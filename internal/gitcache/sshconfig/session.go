package sshconfig

import (
	"errors"
	"os"
	"sort"
	"strings"
)

// EnvironmentVariable values are private session data and must not be logged.
type EnvironmentVariable struct{ Name, Value string }

func (r *resolver) sessionSettings(c *Config) error {
	c.WarnWeakCrypto = r.scalar("warnweakcrypto", "yes")
	switch c.WarnWeakCrypto {
	case "yes", "no", "no-pq-kex":
	default:
		return r.bad("warnweakcrypto", "unsupported warning mode")
	}
	// IgnoreUnknown does not grant permission to ignore recognized but unsupported
	// connection settings. Current WarnWeakCrypto is recognized, so it needs no
	// suppression. Unknown applicable settings still fail explicitly.
	master := r.scalar("controlmaster", "no")
	switch master {
	case "no", "false":
	case "yes", "true", "auto", "ask", "autoask":
		return r.bad("controlmaster", "active connection sharing is unsupported by the native transport")
	default:
		return r.bad("controlmaster", "invalid connection sharing mode")
	}
	persist := r.scalar("controlpersist", "no")
	if persist != "no" && persist != "yes" && !validPersistTime(persist) {
		return r.bad("controlpersist", "unsupported duration syntax")
	}
	raw := r.scalar("controlpath", "none")
	if raw != "none" {
		v := r.values["controlpath"]
		p, err := r.expand(raw, "controlpath", v.source, v.line, c.HostName, c.User, c.Port)
		if err != nil {
			return err
		}
		c.ControlPath = p
		if _, err := os.Lstat(p); err == nil {
			return r.bad("controlpath", "existing control path may select another connection; native multiplexing is unsupported")
		} else if !errors.Is(err, os.ErrNotExist) {
			return r.bad("controlpath", "cannot establish that connection sharing is dormant")
		}
	}
	// With master disabled and no usable control path, OpenSSH creates an
	// independent connection. ControlPersist cannot activate sharing by itself.
	c.ControlPathDormant = true
	c.SendEnv = append([]string(nil), r.sendEnv...)
	if v, ok := r.values["setenv"]; ok {
		for _, assignment := range v.args {
			name, raw, hasEquals := strings.Cut(assignment, "=")
			if !hasEquals || !envName(name) {
				return r.bad("setenv", "expected NAME=value assignments")
			}
			val := ""
			if raw != "" {
				var err error
				val, err = r.expand(raw, "setenv", v.source, v.line, c.HostName, c.User, c.Port)
				if err != nil {
					return err
				}
			}
			c.SetEnv = append(c.SetEnv, EnvironmentVariable{Name: name, Value: val})
		}
	}
	return nil
}

// SessionEnvironment builds the exact selected requests without consulting the
// process environment itself. SetEnv overrides selected inherited values.
// The adapter must send these via SSH env requests before starting upload-pack;
// this helper does not perform remote requests and is not yet wired to the CLI.
func SessionEnvironment(c Config, environ []string) []EnvironmentVariable {
	chosen := map[string]string{}
	for _, entry := range environ {
		name, val, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		for _, pattern := range c.SendEnv {
			if wildcard(name, pattern) {
				chosen[name] = val
				break
			}
		}
	}
	for _, entry := range c.SetEnv {
		chosen[entry.Name] = entry.Value
	}
	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]EnvironmentVariable, 0, len(names))
	for _, name := range names {
		result = append(result, EnvironmentVariable{Name: name, Value: chosen[name]})
	}
	return result
}

func validPersistTime(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for len(s) > 0 {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return false
		}
		s = s[i:]
		if s == "" {
			return true
		}
		if !strings.ContainsRune("sSmMhHdDwW", rune(s[0])) {
			return false
		}
		s = s[1:]
	}
	return true
}
