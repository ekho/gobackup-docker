package render

import (
	"os"
	"reflect"
	"testing"
)

func TestEscapeBareDollars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"m9qq!$7v!s^$!UU", "m9qq!${GB_DOLLAR}7v!s^${GB_DOLLAR}!UU"},                 // $7,$! not names → escaped
		{"${NEXTCLOUD_DB_PASSWORD}", "${NEXTCLOUD_DB_PASSWORD}"},                     // braced ref: untouched
		{"${GB_SHOP_DATABASES_MAIN_PASSWORD}", "${GB_SHOP_DATABASES_MAIN_PASSWORD}"}, // cred placeholder: untouched
		{"$HOME/data", "$HOME/data"},                                                 // bare $NAME ref: NOW preserved
		{"$DB_HOST:5432", "$DB_HOST:5432"},                                           // bare ref with underscore: preserved
		{"end$", "end${GB_DOLLAR}"},                                                  // trailing $ → escaped
		{"no dollars here", "no dollars here"},                                       // unchanged
		{"${PW}_a$7", "${PW}_a${GB_DOLLAR}7"},                                        // braced ref + non-ref $
		{"$A", "${GB_DOLLAR}A"},                                                      // single-char name (< 2) → escaped
		{"${A}", "${GB_DOLLAR}{A}"},                                                  // single-char braced name → escaped
		{"$1abc", "${GB_DOLLAR}1abc"},                                                // digit-led → escaped
		{"a$b c", "a${GB_DOLLAR}b c"},                                                // $b: name < 2 chars → escaped
		{"a$bc d", "a$bc d"},                                                         // $bc: valid 2-char name → preserved
		{"$$", "${GB_DOLLAR}${GB_DOLLAR}"},                                           // consecutive $ → escaped
		{"${unterminated", "${GB_DOLLAR}{unterminated"},                              // no closing brace → escaped
		{"", ""},
	}
	for _, c := range cases {
		if got := EscapeBareDollars(c.in); got != c.want {
			t.Errorf("EscapeBareDollars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The escape must actually survive gobackup's os.ExpandEnv when the sentinel var
// is set — that is the whole point.
func TestEscapeBareDollars_survivesExpandEnv(t *testing.T) {
	t.Setenv(DollarSentinelVar, DollarSentinelValue)
	t.Setenv("PREFIX", "abc")
	cases := []struct{ in, want string }{
		{"m9qq!$7v!s^$!UU", "m9qq!$7v!s^$!UU"},
		{"${PREFIX}_a$7v!s^$!UU", "abc_a$7v!s^$!UU"}, // braced ref expands, literal $ preserved
		{"$PREFIX/x", "abc/x"},                       // bare $NAME ref still expands (preserved, not escaped)
	}
	for _, c := range cases {
		if got := os.ExpandEnv(EscapeBareDollars(c.in)); got != c.want {
			t.Errorf("ExpandEnv(Escape(%q)) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEscapeBareDollars_idempotent(t *testing.T) {
	in := "m9qq!$7v!s^$!UU"
	once := EscapeBareDollars(in)
	if twice := EscapeBareDollars(once); twice != once {
		t.Errorf("not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestEscapeConfig(t *testing.T) {
	cfg := map[string]any{
		"models": map[string]any{
			"shop": map[string]any{
				"databases": map[string]any{
					"main": map[string]any{
						"password": "a$7b",
						"type":     "postgresql",
						"host":     "db",
					},
				},
				"storages": map[string]any{
					"s3": map[string]any{
						"access_key_id": "${YC_ACCESS_KEY}", // intended ref: must NOT be escaped
					},
				},
				"tables": []any{"t$1", "t2"},
			},
		},
	}
	changed := EscapeConfig(cfg)
	if !changed {
		t.Fatal("EscapeConfig returned false; expected it to escape a$7b and t$1")
	}
	shop := cfg["models"].(map[string]any)["shop"].(map[string]any)
	if got := shop["databases"].(map[string]any)["main"].(map[string]any)["password"]; got != "a${GB_DOLLAR}7b" {
		t.Errorf("password = %q, want escaped", got)
	}
	if got := shop["storages"].(map[string]any)["s3"].(map[string]any)["access_key_id"]; got != "${YC_ACCESS_KEY}" {
		t.Errorf("intended ${VAR} was altered: %q", got)
	}
	if got := shop["tables"].([]any)[0]; got != "t${GB_DOLLAR}1" {
		t.Errorf("array element not escaped: %q", got)
	}
}

func TestEscapeConfig_noBareDollar(t *testing.T) {
	cfg := map[string]any{"models": map[string]any{
		"m": map[string]any{"password": "${ONLY_A_REF}", "host": "db", "port": 5432},
	}}
	if EscapeConfig(cfg) {
		t.Error("EscapeConfig returned true for a config with only ${VAR} refs and no bare $")
	}
}

func TestBareDollarPaths(t *testing.T) {
	cfg := map[string]any{"models": map[string]any{
		"shop": map[string]any{
			"databases": map[string]any{"main": map[string]any{
				"password": "a$7b",        // bare $ → reported
				"host":     "db",          // clean
				"token":    "${INTENDED}", // ref → not reported
			}},
		},
	}}
	got := BareDollarPaths(cfg)
	want := []string{"models.shop.databases.main.password"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BareDollarPaths = %v, want %v", got, want)
	}
}
