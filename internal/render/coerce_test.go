package render

import (
	"reflect"
	"testing"
)

func TestCoerceScalar(t *testing.T) {
	tests := []struct {
		key, val string
		want     any
	}{
		{"keep", "90", 90},        // plain int → int
		{"port", "5432", 5432},    // plain int → int
		{"enabled", "true", true}, // bool
		{"on_success", "False", false},
		{"chat_id", "66097481", "66097481"},                     // identifier → stays string
		{"access_key_id", "12345", "12345"},                     // *_id → stays string
		{"token", "999", "999"},                                 // secret-ish → stays string
		{"password", "0000", "0000"},                            // secret → stays string
		{"host", "10", "10"},                                    // host → stays string
		{"path", "/backups/x", "/backups/x"},                    // non-numeric → string
		{"code", "007", "007"},                                  // leading zero → stays string
		{"n", "0", 0},                                           // canonical zero coerces
		{"cron", "0 1 * * *", "0 1 * * *"},                      // has spaces → string
		{"big", "99999999999999999999", "99999999999999999999"}, // overflow → string
	}
	for _, tt := range tests {
		if got := coerceScalar(tt.key, tt.val); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("coerceScalar(%q,%q) = %#v, want %#v", tt.key, tt.val, got, tt.want)
		}
	}
}

func TestCoerceTree_nestedAndLists(t *testing.T) {
	tree := map[string]any{
		"storages": map[string]any{
			"s3": map[string]any{"keep": "10", "bucket": "media.kd"},
		},
		"notifiers": map[string]any{
			"tg": map[string]any{"type": "telegram", "chat_id": "66097481"},
		},
		"archive": map[string]any{"includes": []any{"/a", "/b"}},
	}
	coerceTree(tree)

	s3 := tree["storages"].(map[string]any)["s3"].(map[string]any)
	if s3["keep"] != 10 {
		t.Errorf("keep = %#v, want int 10", s3["keep"])
	}
	if s3["bucket"] != "media.kd" {
		t.Errorf("bucket mutated: %#v", s3["bucket"])
	}
	if cid := tree["notifiers"].(map[string]any)["tg"].(map[string]any)["chat_id"]; cid != "66097481" {
		t.Errorf("chat_id must stay string, got %#v", cid)
	}
	if inc := tree["archive"].(map[string]any)["includes"].([]any); inc[0] != "/a" {
		t.Errorf("list element mutated: %#v", inc)
	}
}

// --- archive.includes / archive.excludes string → array coercion ---

func TestCoerceArchiveIncludes_stringToArray(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"includes": "/var/www,/etc/nginx"},
	}
	coerceTree(tree)
	includes := tree["archive"].(map[string]any)["includes"].([]any)
	if len(includes) != 2 {
		t.Fatalf("len = %d, want 2", len(includes))
	}
	if includes[0] != "/var/www" || includes[1] != "/etc/nginx" {
		t.Errorf("got %#v, want [\"/var/www\", \"/etc/nginx\"]", includes)
	}
}

func TestCoerceArchiveExcludes_stringToArray(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"excludes": "*.log,*.tmp"},
	}
	coerceTree(tree)
	excludes := tree["archive"].(map[string]any)["excludes"].([]any)
	if len(excludes) != 2 {
		t.Fatalf("len = %d, want 2", len(excludes))
	}
	if excludes[0] != "*.log" || excludes[1] != "*.tmp" {
		t.Errorf("got %#v, want [\"*.log\", \"*.tmp\"]", excludes)
	}
}

func TestCoerceArchiveIncludes_trimsSpaces(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"includes": " /var/www , /etc "},
	}
	coerceTree(tree)
	includes := tree["archive"].(map[string]any)["includes"].([]any)
	if includes[0] != "/var/www" || includes[1] != "/etc" {
		t.Errorf("got %#v, want [\"/var/www\", \"/etc\"]", includes)
	}
}

func TestCoerceArchiveIncludes_singleElement(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"includes": "/only"},
	}
	coerceTree(tree)
	includes := tree["archive"].(map[string]any)["includes"].([]any)
	if len(includes) != 1 || includes[0] != "/only" {
		t.Errorf("got %#v, want [\"/only\"]", includes)
	}
}

func TestCoerceArchiveIncludes_emptyString(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"includes": ""},
	}
	coerceTree(tree)
	includes := tree["archive"].(map[string]any)["includes"].([]any)
	if len(includes) != 0 {
		t.Errorf("got %#v, want empty slice", includes)
	}
}

func TestCoerceArchiveIncludes_alreadyArray(t *testing.T) {
	tree := map[string]any{
		"archive": map[string]any{"includes": []any{"/a", "/b"}},
	}
	coerceTree(tree)
	includes := tree["archive"].(map[string]any)["includes"].([]any)
	if len(includes) != 2 || includes[0] != "/a" {
		t.Errorf("array mutated: %#v", includes)
	}
}

func TestCoerceArchiveIncludes_noArchiveKey(t *testing.T) {
	tree := map[string]any{
		"storages": map[string]any{"s3": map[string]any{"type": "s3"}},
	}
	coerceTree(tree)
	// must not panic
}
