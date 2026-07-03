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

// --- databases.*.tables (and other schema array fields) string → array coercion ---

func TestCoerceDatabaseTables_stringToArray(t *testing.T) {
	tree := map[string]any{
		"databases": map[string]any{
			"pg": map[string]any{"type": "postgresql", "tables": "users,orders"},
		},
	}
	coerceTree(tree)
	tables := tree["databases"].(map[string]any)["pg"].(map[string]any)["tables"].([]any)
	if len(tables) != 2 || tables[0] != "users" || tables[1] != "orders" {
		t.Errorf("got %#v, want [\"users\", \"orders\"]", tables)
	}
}

func TestCoerceDatabaseTables_emptyString(t *testing.T) {
	// The user's real config uses `tables: []` — an empty label must render as [].
	tree := map[string]any{
		"databases": map[string]any{
			"pg": map[string]any{"type": "postgresql", "tables": ""},
		},
	}
	coerceTree(tree)
	tables := tree["databases"].(map[string]any)["pg"].(map[string]any)["tables"].([]any)
	if len(tables) != 0 {
		t.Errorf("got %#v, want empty slice", tables)
	}
}

func TestCoerceDatabaseTables_alreadyArray(t *testing.T) {
	tree := map[string]any{
		"databases": map[string]any{
			"pg": map[string]any{"tables": []any{"a", "b"}},
		},
	}
	coerceTree(tree)
	tables := tree["databases"].(map[string]any)["pg"].(map[string]any)["tables"].([]any)
	if len(tables) != 2 || tables[0] != "a" {
		t.Errorf("array mutated: %#v", tables)
	}
}

func TestCoerceArrayFields_pathSpecific(t *testing.T) {
	// Array coercion must be PATH-aware: the same key name outside its schema
	// position must stay a plain string.
	tree := map[string]any{
		"storages": map[string]any{
			"s3": map[string]any{"tables": "a,b", "includes": "x,y"},
		},
		"tables":   "top,level",
		"includes": "also,top",
	}
	coerceTree(tree)
	s3 := tree["storages"].(map[string]any)["s3"].(map[string]any)
	if _, isArr := s3["tables"].([]any); isArr {
		t.Errorf("storages.s3.tables must stay string, got %#v", s3["tables"])
	}
	if _, isArr := s3["includes"].([]any); isArr {
		t.Errorf("storages.s3.includes must stay string, got %#v", s3["includes"])
	}
	if _, isArr := tree["tables"].([]any); isArr {
		t.Errorf("top-level tables must stay string, got %#v", tree["tables"])
	}
	if _, isArr := tree["includes"].([]any); isArr {
		t.Errorf("top-level includes must stay string, got %#v", tree["includes"])
	}
}

func TestCoerceArrayFields_fullSchema(t *testing.T) {
	// All 7 array-typed fields in gobackup's schema (verified against upstream
	// source: GetStringSlice is the only slice accessor repo-wide).
	tests := []struct {
		name string
		tree map[string]any
		get  func(map[string]any) any
	}{
		{"databases.*.exclude_tables", // mysql, postgresql, mongodb
			map[string]any{"databases": map[string]any{"my": map[string]any{"exclude_tables": "logs,cache"}}},
			func(m map[string]any) any {
				return m["databases"].(map[string]any)["my"].(map[string]any)["exclude_tables"]
			}},
		{"databases.*.exclude_tables_prefix", // mongodb
			map[string]any{"databases": map[string]any{"mg": map[string]any{"exclude_tables_prefix": "tmp_,bak_"}}},
			func(m map[string]any) any {
				return m["databases"].(map[string]any)["mg"].(map[string]any)["exclude_tables_prefix"]
			}},
		{"databases.*.skip_databases", // mssql
			map[string]any{"databases": map[string]any{"ms": map[string]any{"skip_databases": "master,tempdb"}}},
			func(m map[string]any) any {
				return m["databases"].(map[string]any)["ms"].(map[string]any)["skip_databases"]
			}},
		{"databases.*.endpoints", // etcd (deprecated upstream, still array-typed)
			map[string]any{"databases": map[string]any{"et": map[string]any{"endpoints": "http://a:2379,http://b:2379"}}},
			func(m map[string]any) any {
				return m["databases"].(map[string]any)["et"].(map[string]any)["endpoints"]
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coerceTree(tt.tree)
			arr, ok := tt.get(tt.tree).([]any)
			if !ok {
				t.Fatalf("%s not coerced to array: %#v", tt.name, tt.get(tt.tree))
			}
			if len(arr) != 2 {
				t.Errorf("len = %d, want 2: %#v", len(arr), arr)
			}
		})
	}
}

func TestCoerce_neverSplitsProtectedStrings(t *testing.T) {
	// Fields that LOOK list-like but must stay strings — coercing them corrupts
	// gobackup behavior (mail splits `to` itself; gcs credentials is JSON with
	// commas; args is one shell fragment).
	tree := map[string]any{
		"databases": map[string]any{"pg": map[string]any{"args": "--if-exists --clean"}},
		"storages":  map[string]any{"g": map[string]any{"type": "gcs", "credentials": `{"a":1,"b":2}`}},
		"notifiers": map[string]any{"m": map[string]any{"type": "mail", "to": "a@x.com,b@y.com"}},
	}
	coerceTree(tree)
	if got := tree["databases"].(map[string]any)["pg"].(map[string]any)["args"]; got != "--if-exists --clean" {
		t.Errorf("databases.args mutated: %#v", got)
	}
	if got := tree["storages"].(map[string]any)["g"].(map[string]any)["credentials"]; got != `{"a":1,"b":2}` {
		t.Errorf("gcs credentials mutated: %#v", got)
	}
	if got := tree["notifiers"].(map[string]any)["m"].(map[string]any)["to"]; got != "a@x.com,b@y.com" {
		t.Errorf("notifiers.to must stay a comma string (mail splits it itself): %#v", got)
	}
}

func TestCoerceDatabaseTables_scalarSiblingsUntouched(t *testing.T) {
	// Coercion of tables must not break sibling scalar coercion in the same map.
	tree := map[string]any{
		"databases": map[string]any{
			"my": map[string]any{"type": "mysql", "tables": "t1", "port": "3306", "password": "123"},
		},
	}
	coerceTree(tree)
	my := tree["databases"].(map[string]any)["my"].(map[string]any)
	if my["port"] != 3306 {
		t.Errorf("port = %#v, want int 3306", my["port"])
	}
	if my["password"] != "123" {
		t.Errorf("password must stay string, got %#v", my["password"])
	}
	if tbl := my["tables"].([]any); len(tbl) != 1 || tbl[0] != "t1" {
		t.Errorf("tables = %#v", my["tables"])
	}
}
