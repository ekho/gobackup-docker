package pipeline

import (
	"reflect"
	"sort"
	"testing"

	"github.com/docker/docker/api/types/network"

	"github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
)

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
	spec := buildGobackupSpec(cfg, existingFixture(), testMounts)

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
	spec := buildGobackupSpec(container.Config{}, existingFixture(), testMounts)

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
	spec := buildGobackupSpec(container.Config{Image: "only:img"}, existingFixture(), testMounts)
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
	spec := buildGobackupSpec(container.Config{}, ex, testMounts)
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label must be forced even when absent: %#v", spec.Labels)
	}
}

func TestBuildGobackupSpec_configLabelsOverrideButComponentStays(t *testing.T) {
	ex := existingFixture()
	ex.Labels["k"] = "old"
	cfg := container.Config{Labels: map[string]string{"k": "new"}}
	spec := buildGobackupSpec(cfg, ex, testMounts)
	if spec.Labels["k"] != "new" {
		t.Errorf("Config label should override existing: %#v", spec.Labels)
	}
	if spec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("component label must survive: %#v", spec.Labels)
	}
}
