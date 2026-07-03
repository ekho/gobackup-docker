package render

import (
	"strconv"
	"strings"
)

// arrayFieldPatterns lists the model-relative config paths that gobackup reads
// as arrays (viper GetStringSlice). Label values are always flat strings, so at
// these paths a comma-separated string is split into a YAML array. "*" matches
// exactly one path segment (the databases id). Everything else is left to scalar
// coercion.
//
// This list is exhaustive for gobackup @ main: GetStringSlice is the only slice
// accessor in the whole config schema. Deliberately NOT here (all confirmed
// non-arrays): databases.*.args (one shell fragment), databases.*.endpoint
// (singular string), notifiers.*.to (each notifier splits it itself),
// storages.*.credentials (JSON with commas), and every *_with.args. See
// docs/ARCHITECTURE.md and the schema audit for evidence.
var arrayFieldPatterns = [][]string{
	{"archive", "includes"},
	{"archive", "excludes"},
	{"databases", "*", "tables"},                // mysql, postgresql
	{"databases", "*", "exclude_tables"},        // mysql, postgresql, mongodb
	{"databases", "*", "exclude_tables_prefix"}, // mongodb
	{"databases", "*", "skip_databases"},        // mssql (when all_databases: true)
	{"databases", "*", "endpoints"},             // etcd (deprecated upstream; prefer singular `endpoint`)
}

// coerceTree normalizes string leaves that clearly represent bools/ints into
// native YAML types, so label values (always strings from Docker) render as
// `keep: 90` / `enabled: true` rather than quoted strings — and converts
// comma-separated strings into arrays at the schema positions gobackup expects
// arrays (arrayFieldPatterns).
//
// It is deliberately conservative: identifier-, secret- and host-like keys are
// left as strings, because values such as a telegram chat_id ("66097481") or a
// numeric token MUST NOT become integers. Numbers with leading zeros are also
// preserved. gobackup itself coerces on read, so this is purely cosmetic — the
// safe default is to leave a value alone when in doubt. Array coercion is
// path-aware: a key named "tables" outside databases.<id> stays a string.
func coerceTree(v any) any {
	return coerceNode(v, nil)
}

func coerceNode(v any, path []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			childPath := append(append([]string(nil), path...), k)
			if s, ok := val.(string); ok {
				if isArrayField(childPath) {
					t[k] = splitCSV(s)
				} else {
					t[k] = coerceScalar(k, s)
				}
			} else {
				coerceNode(val, childPath)
			}
		}
		return t
	case []any:
		for _, val := range t {
			// List elements carry no key context; only recurse into maps.
			if _, ok := val.(string); !ok {
				coerceNode(val, path)
			}
		}
		return t
	default:
		return v
	}
}

// isArrayField reports whether path matches one of arrayFieldPatterns
// ("*" matches exactly one segment).
func isArrayField(path []string) bool {
	for _, pat := range arrayFieldPatterns {
		if len(pat) != len(path) {
			continue
		}
		match := true
		for i, seg := range pat {
			if seg != "*" && seg != path[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// splitCSV splits a comma-separated string, trims whitespace from each item,
// and discards empty items. Returns an []any suitable for YAML marshalling.
func splitCSV(s string) []any {
	if s == "" {
		return []any{}
	}
	raw := strings.Split(s, ",")
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func coerceScalar(key, s string) any {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	if isProtectedKey(key) {
		return s
	}
	if isPlainInt(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(n)
		}
	}
	return s
}

// isProtectedKey reports keys whose values must stay strings even if numeric
// (identifiers, secrets, hostnames).
func isProtectedKey(key string) bool {
	k := strings.ToLower(key)
	if k == "id" || k == "host" || strings.HasSuffix(k, "_id") {
		return true
	}
	for _, frag := range []string{"password", "token", "secret", "key"} {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

// isPlainInt reports a canonical base-10 integer: optional '-', no leading zeros
// (except "0" itself), digits only.
func isPlainInt(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return false
		}
		i = 1
	}
	if s[i] == '0' {
		return len(s)-i == 1 // only "0" / "-0"
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
