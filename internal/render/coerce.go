package render

import (
	"strconv"
	"strings"
)

// coerceTree normalizes string leaves that clearly represent bools/ints into
// native YAML types, so label values (always strings from Docker) render as
// `keep: 90` / `enabled: true` rather than quoted strings.
//
// It is deliberately conservative: identifier-, secret- and host-like keys are
// left as strings, because values such as a telegram chat_id ("66097481") or a
// numeric token MUST NOT become integers. Numbers with leading zeros are also
// preserved. gobackup itself coerces on read, so this is purely cosmetic — the
// safe default is to leave a value alone when in doubt.
func coerceTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				t[k] = coerceScalar(k, s)
			} else {
				coerceTree(val)
			}
		}
		return t
	case []any:
		for _, val := range t {
			// List elements carry no key context; only recurse into maps.
			if _, ok := val.(string); !ok {
				coerceTree(val)
			}
		}
		return t
	default:
		return v
	}
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
