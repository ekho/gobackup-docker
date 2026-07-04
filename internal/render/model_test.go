package render

import (
	"strings"
	"testing"

	"github.com/ekho/gobackup-docker/internal/labels"
	"gopkg.in/yaml.v3"
)

func TestBuildWithCreds(t *testing.T) {
	src := Source{
		Container: "nc", ContainerID: "cid1",
		Model: map[string]any{
			"databases": map[string]any{"db": map[string]any{"type": "postgresql"}},
			"storages":  map[string]any{"local": map[string]any{"type": "local", "path": "/b"}},
		},
		CredRefs: []labels.CredRef{
			{Path: []string{"databases", "db", "password"}, Kind: labels.CredEnv, Ref: "DB_PW"},
			{Path: []string{"storages", "local", "secret_key"}, Kind: labels.CredFile, Ref: "/run/secrets/sk"},
		},
	}
	cfg, creds := BuildWithCreds([]Source{src}, Profiles{}, "h", "")

	m := cfg["models"].(map[string]any)["nc-h"].(map[string]any)
	if got := m["databases"].(map[string]any)["db"].(map[string]any)["password"]; got != "${GB_NC_H_DATABASES_DB_PASSWORD}" {
		t.Errorf("password placeholder = %v", got)
	}
	if got := m["storages"].(map[string]any)["local"].(map[string]any)["secret_key"]; got != "${GB_NC_H_STORAGES_LOCAL_SECRET_KEY}" {
		t.Errorf("secret_key placeholder = %v", got)
	}
	if len(creds) != 2 {
		t.Fatalf("want 2 resolved creds, got %d: %#v", len(creds), creds)
	}
	byVar := map[string]ResolvedCred{}
	for _, c := range creds {
		byVar[c.Var] = c
	}
	if c := byVar["GB_NC_H_DATABASES_DB_PASSWORD"]; c.Kind != labels.CredEnv || c.Ref != "DB_PW" || c.ContainerID != "cid1" {
		t.Errorf("env cred = %#v", c)
	}
	if c := byVar["GB_NC_H_STORAGES_LOCAL_SECRET_KEY"]; c.Kind != labels.CredFile || c.Ref != "/run/secrets/sk" {
		t.Errorf("file cred = %#v", c)
	}
}

func TestBuildWithCreds_conflictAndEmptySkip(t *testing.T) {
	// inline password AND password_env for the same key → skip the model, no creds.
	conflict := Source{
		Container: "x",
		Model: map[string]any{
			"databases": map[string]any{"db": map[string]any{"type": "pg", "password": "inline"}},
			"storages":  map[string]any{"l": map[string]any{"type": "local"}},
		},
		CredRefs: []labels.CredRef{{Path: []string{"databases", "db", "password"}, Kind: labels.CredEnv, Ref: "X"}},
	}
	cfg, creds := BuildWithCreds([]Source{conflict}, Profiles{}, "h", "")
	if len(cfg["models"].(map[string]any)) != 0 {
		t.Error("model with inline+_env conflict must be skipped")
	}
	if len(creds) != 0 {
		t.Errorf("no creds from a skipped model, got %#v", creds)
	}

	// empty reference → skip.
	empty := Source{
		Container: "y",
		Model:     map[string]any{"databases": map[string]any{"db": map[string]any{"type": "pg"}}, "storages": map[string]any{"l": map[string]any{"type": "local"}}},
		CredRefs:  []labels.CredRef{{Path: []string{"databases", "db", "password"}, Kind: labels.CredFile, Ref: ""}},
	}
	cfg2, _ := BuildWithCreds([]Source{empty}, Profiles{}, "h", "")
	if len(cfg2["models"].(map[string]any)) != 0 {
		t.Error("model with empty credential ref must be skipped")
	}
}

func TestBuild_arrayFieldRendersAsYAMLSequence(t *testing.T) {
	// End-to-end: a CSV label value at an array path must marshal to a real YAML
	// block sequence in the generated config, not a quoted scalar gobackup would
	// mis-split.
	src := Source{
		Container: "pg",
		Model: map[string]any{
			"databases": map[string]any{"main": map[string]any{"type": "postgresql", "tables": "users,orders"}},
			"storages":  map[string]any{"local": map[string]any{"type": "local", "path": "/b"}},
		},
	}
	out, err := yaml.Marshal(Build([]Source{src}, Profiles{}, "h", ""))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "- users") || !strings.Contains(got, "- orders") {
		t.Errorf("tables did not render as a YAML sequence:\n%s", got)
	}
	if strings.Contains(got, "tables: users") {
		t.Errorf("tables rendered as a scalar string:\n%s", got)
	}
}

func TestModelName(t *testing.T) {
	tests := []struct {
		container, custom, host, want string
	}{
		{"gitea", "", "h1", "gitea-h1"},       // auto: container-host
		{"gitea", "mymodel", "h1", "mymodel"}, // explicit name wins, host ignored
		{"gitea", "", "", "gitea"},            // no host: just container
		{"gitea", "mymodel", "", "mymodel"},   // explicit name, no host
	}
	for _, tt := range tests {
		if got := ModelName(tt.container, tt.custom, tt.host); got != tt.want {
			t.Errorf("ModelName(%q,%q,%q) = %q, want %q", tt.container, tt.custom, tt.host, got, tt.want)
		}
	}
}

func TestValidateModel(t *testing.T) {
	tests := []struct {
		name    string
		model   map[string]any
		wantErr bool
	}{
		{"no storages", map[string]any{"databases": map[string]any{"d": map[string]any{}}}, true},
		{"storages but no db/archive", map[string]any{"storages": map[string]any{"s": map[string]any{}}}, true},
		{"storages + databases", map[string]any{"storages": map[string]any{"s": map[string]any{}}, "databases": map[string]any{"d": map[string]any{}}}, false},
		{"storages + archive", map[string]any{"storages": map[string]any{"s": map[string]any{}}, "archive": map[string]any{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModel(tt.model)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateModel err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func profilesFixture() Profiles {
	return Profiles{
		"default": {
			"schedule":        map[string]any{"cron": "0 1 * * *"},
			"default_storage": "s3",
			"storages": map[string]any{
				"local": map[string]any{"type": "local", "keep": 10, "path": "/b/{{ .Model }}"},
				"s3":    map[string]any{"type": "s3", "keep": 10, "path": "/b/{{ .Model }}"},
			},
			"notifiers": map[string]any{"tg": map[string]any{"type": "telegram"}},
		},
	}
}

func TestBuild_inheritOverrideOptoutTemplate(t *testing.T) {
	src := Source{
		Container: "gitea",
		Model: map[string]any{
			"databases": map[string]any{"gitea": map[string]any{"type": "postgresql"}},
			"storages":  map[string]any{"s3": map[string]any{"keep": "90"}}, // override just keep
			"notifiers": labels.OptOut,                                      // opt out inherited notifiers
		},
	}
	cfg := Build([]Source{src}, profilesFixture(), "h1", "")
	models := cfg["models"].(map[string]any)

	m, ok := models["gitea-h1"].(map[string]any)
	if !ok {
		t.Fatalf("model gitea-h1 missing; models=%#v", models)
	}
	if _, hasNotifiers := m["notifiers"]; hasNotifiers {
		t.Error("notifiers should have been opted out")
	}
	s3 := m["storages"].(map[string]any)["s3"].(map[string]any)
	if s3["keep"] != 90 || s3["type"] != "s3" { // override applied (coerced to int), type inherited
		t.Errorf("s3 = %#v", s3)
	}
	local := m["storages"].(map[string]any)["local"].(map[string]any)
	if local["keep"] != 10 { // untouched inherited value keeps its int type
		t.Errorf("local.keep = %#v, want int 10", local["keep"])
	}
	if local["path"] != "/b/gitea-h1" { // template expanded with final name
		t.Errorf("local.path = %q", local["path"])
	}
}

func TestBuild_profileIsolation(t *testing.T) {
	// Two containers inherit the same profile; an override on one must not bleed
	// into the other (regression guard for deepCopy in Build).
	profiles := profilesFixture()
	srcs := []Source{
		{Container: "a", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "x"}}, "storages": map[string]any{"s3": map[string]any{"keep": "999"}}}},
		{Container: "b", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "x"}}}},
	}
	models := Build(srcs, profiles, "h", "")["models"].(map[string]any)
	bS3 := models["b-h"].(map[string]any)["storages"].(map[string]any)["s3"].(map[string]any)
	if bS3["keep"] != 10 {
		t.Errorf("profile leaked: b.s3.keep = %#v, want int 10", bS3["keep"])
	}
	// And the profile fixture itself must be pristine.
	if profiles["default"]["storages"].(map[string]any)["s3"].(map[string]any)["keep"] != 10 {
		t.Error("Build mutated the shared profile")
	}
}

func TestBuild_skips(t *testing.T) {
	profiles := profilesFixture()
	srcs := []Source{
		{Container: "known", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "x"}}}},
		{Container: "badprofile", Profile: "nope", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "x"}}}},
		// label-only (no default lookup issue) but invalid: no storages, no db/archive
		{Container: "invalid", Profile: "", Model: map[string]any{"compress_with": map[string]any{"type": "tgz"}}},
	}
	// Give the invalid one an empty base by using a profile set without "default".
	models := Build(srcs, profiles, "h", "")["models"].(map[string]any)

	if _, ok := models["known-h"]; !ok {
		t.Error("valid model 'known-h' should be present")
	}
	if _, ok := models["badprofile-h"]; ok {
		t.Error("model with unknown profile must be skipped")
	}
}

func TestBuild_collision(t *testing.T) {
	// Same explicit name from two containers → second skipped, first kept.
	profiles := profilesFixture()
	srcs := []Source{
		{Container: "c1", Name: "dup", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "a"}}}},
		{Container: "c2", Name: "dup", Model: map[string]any{"databases": map[string]any{"d": map[string]any{"type": "b"}}}},
	}
	models := Build(srcs, profiles, "h", "")["models"].(map[string]any)
	if len(models) != 1 {
		t.Fatalf("expected 1 model after collision, got %d: %#v", len(models), models)
	}
	dup := models["dup"].(map[string]any)["databases"].(map[string]any)["d"].(map[string]any)
	if dup["type"] != "a" {
		t.Errorf("first writer should win, got type=%v", dup["type"])
	}
}

func TestBuild_archiveIncludesCommaString(t *testing.T) {
	// archive.includes/archive.excludes as comma-separated string (from Docker label)
	// must be coerced to a proper YAML array.
	profiles := profilesFixture()
	src := Source{
		Container: "nextcloud",
		Model: map[string]any{
			"databases": map[string]any{"db": map[string]any{"type": "postgresql"}},
			"archive":   map[string]any{"includes": "/var/www/html,/etc/nginx", "excludes": "*.log,*.tmp"},
		},
	}
	models := Build([]Source{src}, profiles, "h1", "")["models"].(map[string]any)
	m := models["nextcloud-h1"].(map[string]any)
	arch := m["archive"].(map[string]any)

	includes, ok := arch["includes"].([]any)
	if !ok {
		t.Fatalf("archive.includes is %T, want []any", arch["includes"])
	}
	if len(includes) != 2 || includes[0] != "/var/www/html" || includes[1] != "/etc/nginx" {
		t.Errorf("includes = %#v, want [\"/var/www/html\", \"/etc/nginx\"]", includes)
	}

	excludes, ok := arch["excludes"].([]any)
	if !ok {
		t.Fatalf("archive.excludes is %T, want []any", arch["excludes"])
	}
	if len(excludes) != 2 || excludes[0] != "*.log" || excludes[1] != "*.tmp" {
		t.Errorf("excludes = %#v, want [\"*.log\", \"*.tmp\"]", excludes)
	}
}

func TestBuild_labelOnlyNoDefault(t *testing.T) {
	// No profiles at all + no explicit profile => empty base (label-only). A fully
	// self-described model must still build.
	src := Source{
		Container: "solo",
		Model: map[string]any{
			"databases": map[string]any{"d": map[string]any{"type": "postgresql"}},
			"storages":  map[string]any{"local": map[string]any{"type": "local", "path": "/b/{{ .Model }}"}},
		},
	}
	models := Build([]Source{src}, Profiles{}, "h", "")["models"].(map[string]any)
	m, ok := models["solo-h"].(map[string]any)
	if !ok {
		t.Fatalf("label-only model missing; models=%#v", models)
	}
	if got := m["storages"].(map[string]any)["local"].(map[string]any)["path"]; got != "/b/solo-h" {
		t.Errorf("path = %q", got)
	}
}
