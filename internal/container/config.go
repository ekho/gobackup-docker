// Package container parses the gobackup_container.* label namespace into a
// ContainerConfig that describes how to create the managed gobackup container.
//
// Grammar:
//
//	gobackup_container.image                         = "huacnlee/gobackup:latest"
//	gobackup_container.command                       = "run -c /etc/gobackup/gobackup.yml"
//	gobackup_container.networks                      = "backup_net,caddy_net"
//	gobackup_container.env.<VARNAME>                 = "value"        → FOO=bar
//	gobackup_container.labels.<arbitrary>            = "..."          → passthrough
package container

import (
	"strings"
)

const prefix = "gobackup_container."

// Config is the decoded gobackup_container.* label surface. Each field may be
// empty-string / nil when the label was absent; the caller applies defaults.
type Config struct {
	Image    string
	Command  string            // raw string, caller splits into []string via shell-like rules
	Env      []string          // ["FOO=bar", "BAZ=qux"] — one per gobackup_container.env.<key>
	Networks []string          // network names from comma-separated value
	Labels   map[string]string // passthrough labels (under gobackup_container.labels.*)
}

// Parse extracts gobackup_container.* labels. Unknown sub-namespaces are
// silently ignored; only the documented grammar above is read.
func Parse(labels map[string]string) Config {
	cfg := Config{
		Labels: map[string]string{},
	}
	for k, v := range labels {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || rest == "" {
			continue
		}
		// Dispatch on the first segment of the remaining path.
		first, tail, _ := strings.Cut(rest, ".")
		switch first {
		case "image":
			cfg.Image = v
		case "command":
			cfg.Command = v
		case "networks":
			cfg.Networks = splitCSV(v)
		case "env":
			if tail != "" {
				cfg.Env = append(cfg.Env, tail+"="+v)
			}
		case "labels":
			if tail != "" {
				cfg.Labels[tail] = v
			}
		}
	}
	return cfg
}

// splitCSV splits a comma-separated string, trims whitespace, discards empties.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
