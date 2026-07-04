package labels

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_meta(t *testing.T) {
	tests := []struct {
		name             string
		labels           map[string]string
		exposedByDefault bool
		wantEnabled      bool
		wantName         string
		wantInstance     string
		wantProfile      string
	}{
		{
			name:        "explicit enable true",
			labels:      map[string]string{"gobackup.enable": "true"},
			wantEnabled: true,
		},
		{
			name:             "no enable label falls back to exposedByDefault=true",
			labels:           map[string]string{"gobackup.name": "x"},
			exposedByDefault: true,
			wantEnabled:      true,
			wantName:         "x",
		},
		{
			name:             "no enable label falls back to exposedByDefault=false",
			labels:           map[string]string{"gobackup.name": "x"},
			exposedByDefault: false,
			wantEnabled:      false,
			wantName:         "x",
		},
		{
			name:             "explicit enable=false overrides exposedByDefault=true",
			labels:           map[string]string{"gobackup.enable": "false"},
			exposedByDefault: true,
			wantEnabled:      false,
		},
		{
			name:        "lenient bool: on/yes/1",
			labels:      map[string]string{"gobackup.enable": "ON"},
			wantEnabled: true,
		},
		{
			name:             "unrecognised bool uses fallback",
			labels:           map[string]string{"gobackup.enable": "maybe"},
			exposedByDefault: true,
			wantEnabled:      true,
		},
		{
			name: "all meta keys",
			labels: map[string]string{
				"gobackup.enable":   "true",
				"gobackup.name":     "mymodel",
				"gobackup.instance": "prod",
				"gobackup.profile":  "heavy",
			},
			wantEnabled:  true,
			wantName:     "mymodel",
			wantInstance: "prod",
			wantProfile:  "heavy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.labels, tt.exposedByDefault)
			if got.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tt.wantEnabled)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Instance != tt.wantInstance {
				t.Errorf("Instance = %q, want %q", got.Instance, tt.wantInstance)
			}
			if got.Profile != tt.wantProfile {
				t.Errorf("Profile = %q, want %q", got.Profile, tt.wantProfile)
			}
		})
	}
}

func TestParse_modelTree(t *testing.T) {
	labels := map[string]string{
		"gobackup.enable":            "true",
		"gobackup.name":              "should-not-appear-in-model",
		"gobackup.databases.nc.type": "postgresql",
		"gobackup.databases.nc.host": "${NC_HOST}",
		"gobackup.databases.nc.args": "--clean",
		"gobackup.archive.includes":  "/data",
		"gobackup.storages.s3.keep":  "90",
		"gobackup.notifiers":         OptOut, // subtree opt-out kept as sentinel string
		"unrelated.label":            "ignored",
		"com.docker.compose.project": "ignored",
	}
	got := Parse(labels, false)

	want := map[string]any{
		"databases": map[string]any{
			"nc": map[string]any{
				"type": "postgresql",
				"host": "${NC_HOST}",
				"args": "--clean",
			},
		},
		"archive":   map[string]any{"includes": "/data"},
		"storages":  map[string]any{"s3": map[string]any{"keep": "90"}},
		"notifiers": OptOut,
	}
	if !reflect.DeepEqual(got.Model, want) {
		t.Errorf("Model tree mismatch:\n got  = %#v\n want = %#v", got.Model, want)
	}
	// Reserved meta keys must never leak into the model body.
	for _, k := range []string{"enable", "name", "instance", "profile"} {
		if _, ok := got.Model[k]; ok {
			t.Errorf("reserved key %q leaked into model tree", k)
		}
	}
}

func TestParse_credRefs(t *testing.T) {
	labels := map[string]string{
		"gobackup.enable":                        "true",
		"gobackup.databases.nc.type":             "postgresql",
		"gobackup.databases.nc.password_env":     "DB_PASSWORD", // → env credref
		"gobackup.storages.s3.type":              "s3",
		"gobackup.storages.s3.secret_key_file":   "/run/secrets/s3sk", // → file credref (base secret_key)
		"gobackup.storages.gcs.credentials_file": "/keys/gcs.json",    // NOT a credref (real gobackup key)
	}
	got := Parse(labels, false)

	// Two credrefs extracted.
	byKey := map[string]CredRef{}
	for _, c := range got.CredRefs {
		byKey[strings.Join(c.Path, ".")] = c
	}
	if len(got.CredRefs) != 2 {
		t.Fatalf("want 2 credrefs, got %d: %#v", len(got.CredRefs), got.CredRefs)
	}
	if c := byKey["databases.nc.password"]; c.Kind != CredEnv || c.Ref != "DB_PASSWORD" {
		t.Errorf("password credref = %#v", c)
	}
	if c := byKey["storages.s3.secret_key"]; c.Kind != CredFile || c.Ref != "/run/secrets/s3sk" {
		t.Errorf("secret_key credref = %#v", c)
	}

	// Credential leaves must NOT appear in the model tree.
	nc := got.Model["databases"].(map[string]any)["nc"].(map[string]any)
	if _, ok := nc["password_env"]; ok {
		t.Error("password_env leaked into model tree")
	}
	if _, ok := nc["password"]; ok {
		t.Error("credref must not pre-set the model value (render substitutes a placeholder)")
	}
	// gcs.credentials_file is a REAL gobackup key — must stay in the model, untouched.
	gcs := got.Model["storages"].(map[string]any)["gcs"].(map[string]any)
	if gcs["credentials_file"] != "/keys/gcs.json" {
		t.Errorf("credentials_file must NOT be hijacked as a credref: %#v", gcs)
	}
}

func TestParse_ignoresPrefixOnly(t *testing.T) {
	got := Parse(map[string]string{"gobackup.": "x", "gobackup": "y"}, false)
	if len(got.Model) != 0 {
		t.Errorf("bare prefix should produce no model keys, got %#v", got.Model)
	}
}
