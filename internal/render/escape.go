package render

import (
	"fmt"
	"sort"
	"strings"
)

// gobackup loads its config by running os.ExpandEnv over the ENTIRE raw file
// text before parsing the YAML (config/config.go:
// `viper.ReadConfig(strings.NewReader(os.ExpandEnv(string(cfg))))`). So any '$'
// that forms a $name / ${name} / $<shell-special> sequence is substituted —
// usually to empty — silently corrupting the value. A password like
// `m9qq!$7v!s^$!UU` becomes `m9qq!v!s^UU`. YAML quoting cannot help: expansion
// happens on the raw bytes before the parser sees the quotes.
//
// os.ExpandEnv has NO escape character ($$ → "", \$ → "\"), so a literal '$'
// cannot be written inline. Instead we route it through a sentinel env var whose
// value IS '$': `${GB_DOLLAR}` expands back to a single '$'. The supervisor
// injects GB_DOLLAR=$ into the engine's environment (see the pipeline). This is
// the same single-pass property that makes _env credentials safe, reused as an
// escape primitive — os.ExpandEnv does not re-scan a substituted value, so the
// emitted '$' is left alone.
//
// A '$' is treated as an intended variable reference (and left for gobackup to
// expand) ONLY when it is immediately followed by a name of the form
// [a-zA-Z][a-zA-Z0-9_]+ — bare ($NAME) or braced (${NAME}). Note the trailing
// '+': the name is at least two characters (a leading letter plus one or more
// letters/digits/underscores). Every other '$' — before a digit, punctuation, a
// space, end-of-string, or a name too short/ill-formed — is escaped.
const (
	DollarSentinelVar   = "GB_DOLLAR"
	DollarSentinelValue = "$"
)

var dollarSentinelRef = "${" + DollarSentinelVar + "}"

func isLetter(c byte) bool   { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isNameByte(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' || c == '_' }

// refPrefixLen returns the length of a variable reference at the START of s (the
// text immediately after a '$'), or 0 if s does not begin with one. A reference
// is a name matching [a-zA-Z][a-zA-Z0-9_]+ (a letter plus at least one more name
// byte), optionally wrapped in braces: NAME or {NAME}.
func refPrefixLen(s string) int {
	if len(s) == 0 {
		return 0
	}
	if s[0] == '{' {
		if len(s) < 2 || !isLetter(s[1]) {
			return 0
		}
		j := 2
		for j < len(s) && isNameByte(s[j]) {
			j++
		}
		if j >= 3 && j < len(s) && s[j] == '}' { // ≥2 name bytes, then '}'
			return j + 1 // include the closing brace
		}
		return 0
	}
	if isLetter(s[0]) {
		j := 1
		for j < len(s) && isNameByte(s[j]) {
			j++
		}
		if j >= 2 { // leading letter + at least one more name byte
			return j
		}
	}
	return 0
}

// hasBareDollar reports whether s contains a '$' that does NOT begin a variable
// reference — i.e. a '$' that would be escaped (and that gobackup's os.ExpandEnv
// would otherwise corrupt).
func hasBareDollar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && refPrefixLen(s[i+1:]) == 0 {
			return true
		}
	}
	return false
}

// EscapeBareDollars rewrites every '$' that does NOT begin a variable reference
// ($NAME or ${NAME}, NAME = [a-zA-Z][a-zA-Z0-9_]+) into a reference to the
// sentinel var, so gobackup's whole-file os.ExpandEnv leaves it literal. Real
// references are preserved and still expand. The transform is idempotent: the
// emitted `${GB_DOLLAR}` is itself a valid reference, so a second pass leaves it
// untouched.
func EscapeBareDollars(s string) string {
	if !hasBareDollar(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(dollarSentinelRef))
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			if n := refPrefixLen(s[i+1:]); n > 0 {
				// intended reference — copy '$' and the whole name verbatim
				b.WriteByte('$')
				b.WriteString(s[i+1 : i+1+n])
				i += n
				continue
			}
			b.WriteString(dollarSentinelRef)
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// EscapeConfig walks a decoded config document and escapes every string leaf in
// place via EscapeBareDollars, returning true if any value changed (i.e. the
// sentinel var must be provided to the engine). Maps and slices are recursed;
// non-string scalars are left as-is.
//
// LIMITATION: a '$' followed by a valid name ($word or ${word}) is treated as an
// intended reference and left to expand — it cannot be distinguished from a real
// variable the feature must support (e.g. storage ${YC_ACCESS_KEY} or a bare
// $HOME). So a value that genuinely contains such a sequence (a password like
// `p$word` or `Se${cret}`) is left untouched and gobackup expands it (usually to
// empty). Such values must be supplied via a *_env/*_file credential label,
// which never round-trips through os.ExpandEnv. hasBareDollar mirrors this, so
// BareDollarPaths does not flag those either.
func EscapeConfig(cfg map[string]any) bool {
	_, changed := escapeValue(cfg)
	return changed
}

func escapeValue(node any) (any, bool) {
	switch v := node.(type) {
	case string:
		e := EscapeBareDollars(v)
		return e, e != v
	case map[string]any:
		changed := false
		for k, val := range v {
			if nv, c := escapeValue(val); c {
				v[k] = nv
				changed = true
			}
		}
		return v, changed
	case []any:
		changed := false
		for i, val := range v {
			if nv, c := escapeValue(val); c {
				v[i] = nv
				changed = true
			}
		}
		return v, changed
	default:
		return node, false
	}
}

// BareDollarPaths returns the sorted dotted paths of string leaves that contain
// a '$' that would be escaped. Used to warn in label-only mode, where the
// supervisor does not manage the engine and so cannot inject the sentinel var.
func BareDollarPaths(cfg map[string]any) []string {
	var out []string
	collectBareDollarPaths("", cfg, &out)
	sort.Strings(out)
	return out
}

func collectBareDollarPaths(prefix string, node any, out *[]string) {
	switch v := node.(type) {
	case string:
		if hasBareDollar(v) {
			*out = append(*out, prefix)
		}
	case map[string]any:
		for k, val := range v {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			collectBareDollarPaths(p, val, out)
		}
	case []any:
		for i, val := range v {
			collectBareDollarPaths(fmt.Sprintf("%s[%d]", prefix, i), val, out)
		}
	}
}
