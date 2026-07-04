package container

import (
	"testing"
)

func TestParse_image(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.image": "huacnlee/gobackup:v2",
	})
	if cfg.Image != "huacnlee/gobackup:v2" {
		t.Errorf("image = %q", cfg.Image)
	}
}

func TestParse_command(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.command": "run --verbose",
	})
	if cfg.Command != "run --verbose" {
		t.Errorf("command = %q", cfg.Command)
	}
}

func TestParse_networks(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.networks": "backup_net, caddy_net",
	})
	if len(cfg.Networks) != 2 || cfg.Networks[0] != "backup_net" || cfg.Networks[1] != "caddy_net" {
		t.Errorf("networks = %#v", cfg.Networks)
	}
}

func TestParse_env(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.env.FOO":     "bar",
		"gobackup_container.env.SECRET":  "s3cr3t",
		"gobackup_container.networks":    "ignored", // non-env label ignored in Env
	})
	if len(cfg.Env) != 2 {
		t.Fatalf("expected 2 env vars, got %#v", cfg.Env)
	}
	if cfg.Env[0] != "FOO=bar" && cfg.Env[1] != "FOO=bar" {
		t.Errorf("FOO=bar not found in %#v", cfg.Env)
	}
	if cfg.Env[0] != "SECRET=s3cr3t" && cfg.Env[1] != "SECRET=s3cr3t" {
		t.Errorf("SECRET=s3cr3t not found in %#v", cfg.Env)
	}
}

func TestParse_labelsPassthrough(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.labels.caddy_0":               "gobackup.example.com",
		"gobackup_container.labels.caddy_0.reverse_proxy": "{{upstreams 2703}}",
	})
	if cfg.Labels["caddy_0"] != "gobackup.example.com" {
		t.Errorf("caddy_0 = %q", cfg.Labels["caddy_0"])
	}
	if cfg.Labels["caddy_0.reverse_proxy"] != "{{upstreams 2703}}" {
		t.Errorf("caddy_0.reverse_proxy = %q", cfg.Labels["caddy_0.reverse_proxy"])
	}
}

func TestParse_absentFieldsAreEmpty(t *testing.T) {
	cfg := Parse(map[string]string{"gobackup.enable": "true", "other.stuff": "x"})
	if cfg.Image != "" || cfg.Command != "" || cfg.Networks != nil || cfg.Env != nil || len(cfg.Labels) != 0 {
		t.Errorf("all fields should be zero: %#v", cfg)
	}
}

func TestParse_envOnly(t *testing.T) {
	cfg := Parse(map[string]string{
		"gobackup_container.env.DB":        "postgres",
		"gobackup_container.labels.caddy": "x",
	})
	if len(cfg.Env) != 1 || cfg.Env[0] != "DB=postgres" {
		t.Errorf("env = %#v", cfg.Env)
	}
	if cfg.Labels["caddy"] != "x" {
		t.Errorf("labels = %#v", cfg.Labels)
	}
}
