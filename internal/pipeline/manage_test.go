package pipeline

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"

	"github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
)

func TestWrapCommandWithSecrets(t *testing.T) {
	base := []string{"/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"}

	// No exports → base returned unchanged (no shell wrapper).
	if got := wrapCommandWithSecrets(base, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("no exports should return base unchanged, got %#v", got)
	}

	// One secret export → sh -c wrapper with export + exec.
	got := wrapCommandWithSecrets(base, []secretExport{{Var: "GB_A_PW", Path: "/run/secrets/a"}})
	if len(got) != 3 || got[0] != "/bin/sh" || got[1] != "-c" {
		t.Fatalf("expected [/bin/sh -c ...], got %#v", got)
	}
	script := got[2]
	for _, want := range []string{
		`export GB_A_PW="$(cat '/run/secrets/a')"`,
		`exec '/usr/local/bin/gobackup' 'run' '-c' '/etc/gobackup/gobackup.yml'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if !strings.HasPrefix(script, "export ") || !strings.Contains(script, "; exec ") {
		t.Errorf("export must precede exec: %s", script)
	}
}

func TestWrapCommandWithSecrets_injectionSafe(t *testing.T) {
	// A malicious path must be single-quote-escaped, never break out of the quotes.
	evil := `/run/secrets/a'; rm -rf / #`
	got := wrapCommandWithSecrets([]string{"/bin/gobackup", "run"}, []secretExport{{Var: "GB_X", Path: evil}})
	script := got[2]
	if strings.Contains(script, "'; rm -rf / #'") {
		// this exact substring appearing UNescaped (as its own quoted segment) would be a breakout
	}
	// The dangerous sequence must be neutralised: a raw "; rm -rf /" not inside an escaped quote.
	if strings.Contains(script, `cat '/run/secrets/a'; rm -rf`) {
		t.Errorf("path escaped incorrectly — command injection possible:\n%s", script)
	}
	// Correct POSIX single-quote escaping turns ' into '\'' .
	if !strings.Contains(script, `'\''`) {
		t.Errorf("expected single-quote escaping ('\\''), got:\n%s", script)
	}
}

func TestBuildGobackupSpec_credsWrapAndIdempotent(t *testing.T) {
	ex := existingFixture()
	envVars := []string{"GB_A=v1"}
	exports := []secretExport{{Var: "GB_B", Path: "/gobackup-secrets/GB_B"}}

	spec := buildGobackupSpec(container.Config{}, ex, testMounts, envVars, exports)

	if len(spec.Command) != 3 || spec.Command[0] != "/bin/sh" {
		t.Fatalf("command should be wrapped, got %#v", spec.Command)
	}
	if !strings.Contains(spec.Command[2], `export GB_B="$(cat '/gobackup-secrets/GB_B')"`) ||
		!strings.Contains(spec.Command[2], `exec '/usr/local/bin/gobackup' 'run'`) {
		t.Errorf("wrapper script wrong: %s", spec.Command[2])
	}
	if !containsString(spec.Env, "GB_A=v1") || !containsString(spec.Env, "OLD=1") {
		t.Errorf("env should merge cred var with base: %#v", spec.Env)
	}
	if base, ok := decodeBaseCmd(spec.Labels[baseCmdLabel]); !ok || !reflect.DeepEqual(base, ex.Command) {
		t.Errorf("base-cmd label should record the unwrapped base %#v, got %q", ex.Command, spec.Labels[baseCmdLabel])
	}

	// Feed the recreated container back in — must NOT double-wrap.
	ex2 := docker.InspectResult{
		Name: ex.Name, Image: spec.Image, Command: spec.Command,
		Env: spec.Env, Labels: spec.Labels, Networks: ex.Networks,
	}
	spec2 := buildGobackupSpec(container.Config{}, ex2, testMounts, envVars, exports)
	if !reflect.DeepEqual(spec2.Command, spec.Command) {
		t.Errorf("command double-wrapped on re-reconcile:\n1: %#v\n2: %#v", spec.Command, spec2.Command)
	}
}

func existingFixture() docker.InspectResult {
	return docker.InspectResult{
		ID:      "old123456789",
		Name:    "proj-gobackup-1",
		Image:   "huacnlee/gobackup:latest",
		Command: []string{"/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"},
		Env:     []string{"OLD=1"},
		Labels: map[string]string{
			gobackupComponentLabel:       gobackupComponentValue,
			"com.docker.compose.project": "proj",
		},
		Networks: map[string]*network.EndpointSettings{"backup_net": {}},
	}
}

var testMounts = []docker.MountDef{{Source: "vol", Target: "/volumes/x", ReadOnly: true}}

func TestBuildGobackupSpec_configWins(t *testing.T) {
	cfg := container.Config{
		Image:    "myimg:1",
		Command:  "run -c /custom.yml",
		Env:      []string{"FOO=bar"},
		Networks: []string{"netA", "netB"},
		Labels:   map[string]string{"extra": "y"},
	}
	spec := buildGobackupSpec(cfg, existingFixture(), testMounts, nil, nil)

	if spec.Image != "myimg:1" {
		t.Errorf("Image = %q, want myimg:1", spec.Image)
	}
	if !reflect.DeepEqual(spec.Command, []string{"run", "-c", "/custom.yml"}) {
		t.Errorf("Command = %#v, want [run -c /custom.yml] split", spec.Command)
	}
	if !reflect.DeepEqual(spec.Env, []string{"FOO=bar"}) {
		t.Errorf("Env = %#v, want [FOO=bar] (replace, not merge)", spec.Env)
	}
	got := append([]string(nil), spec.Networks...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"netA", "netB"}) {
		t.Errorf("Networks = %#v, want [netA netB]", spec.Networks)
	}
	if spec.Labels["extra"] != "y" {
		t.Errorf("passthrough label missing: %#v", spec.Labels)
	}
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label must be forced: %#v", spec.Labels)
	}
	if spec.Name != "proj-gobackup-1" {
		t.Errorf("Name = %q, want existing name preserved", spec.Name)
	}
	if !reflect.DeepEqual(spec.Mounts, testMounts) {
		t.Errorf("Mounts = %#v, want desired mounts", spec.Mounts)
	}
}

func TestBuildGobackupSpec_emptyConfigFallsBack(t *testing.T) {
	spec := buildGobackupSpec(container.Config{}, existingFixture(), testMounts, nil, nil)

	ex := existingFixture()
	if spec.Image != ex.Image {
		t.Errorf("Image = %q, want fallback %q", spec.Image, ex.Image)
	}
	if !reflect.DeepEqual(spec.Command, ex.Command) {
		t.Errorf("Command = %#v, want fallback %#v", spec.Command, ex.Command)
	}
	if !reflect.DeepEqual(spec.Env, ex.Env) {
		t.Errorf("Env = %#v, want fallback %#v", spec.Env, ex.Env)
	}
	if !reflect.DeepEqual(spec.Networks, []string{"backup_net"}) {
		t.Errorf("Networks = %#v, want [backup_net]", spec.Networks)
	}
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label missing: %#v", spec.Labels)
	}
	if spec.Labels["com.docker.compose.project"] != "proj" {
		t.Errorf("existing labels should be preserved on fallback: %#v", spec.Labels)
	}
}

func TestBuildGobackupSpec_partialImageOnly(t *testing.T) {
	spec := buildGobackupSpec(container.Config{Image: "only:img"}, existingFixture(), testMounts, nil, nil)
	if spec.Image != "only:img" {
		t.Errorf("Image = %q, want only:img", spec.Image)
	}
	if !reflect.DeepEqual(spec.Command, existingFixture().Command) {
		t.Errorf("Command should fall back when Config.Command empty: %#v", spec.Command)
	}
	if !reflect.DeepEqual(spec.Env, []string{"OLD=1"}) {
		t.Errorf("Env should fall back when Config.Env empty: %#v", spec.Env)
	}
}

func TestBuildGobackupSpec_forcesComponentLabelEvenIfAbsent(t *testing.T) {
	// Neither existing nor Config carries the component label → builder must add
	// it, or the recreated container can't be re-discovered next reconcile.
	ex := existingFixture()
	delete(ex.Labels, gobackupComponentLabel)
	spec := buildGobackupSpec(container.Config{}, ex, testMounts, nil, nil)
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label must be forced even when absent: %#v", spec.Labels)
	}
}

func TestBuildGobackupSpec_configLabelsOverrideButComponentStays(t *testing.T) {
	ex := existingFixture()
	ex.Labels["k"] = "old"
	cfg := container.Config{Labels: map[string]string{"k": "new"}}
	spec := buildGobackupSpec(cfg, ex, testMounts, nil, nil)
	if spec.Labels["k"] != "new" {
		t.Errorf("Config label should override existing: %#v", spec.Labels)
	}
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label must survive: %#v", spec.Labels)
	}
}
