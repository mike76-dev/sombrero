package stores

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestBufferAgeYAML verifies how the buffer age is read from and written to the
// config file, in particular that leaving it out means never.
func TestBufferAgeYAML(t *testing.T) {
	tests := []struct {
		yaml string
		want BufferAge
		fail bool
	}{
		{yaml: "maxBufferAge: 24h", want: BufferAge(24 * time.Hour)},
		{yaml: "maxBufferAge: 30m", want: BufferAge(30 * time.Minute)},
		{yaml: "maxBufferAge: never", want: 0},
		{yaml: "maxBufferAge: Never", want: 0},
		{yaml: "maxBufferAge: ''", want: 0},
		{yaml: "maxBufferAge: '  24h  '", want: BufferAge(24 * time.Hour)},
		{yaml: "maxBufferAge: '  never  '", want: 0},
		{yaml: "appName: Sombrero", want: 0},
		{yaml: "maxBufferAge: 0", want: 0},
		{yaml: "maxBufferAge: 0s", want: 0},
		{yaml: "maxBufferAge: 86400", fail: true},
		{yaml: "maxBufferAge: -1h", fail: true},
		{yaml: "maxBufferAge: soon", fail: true},
	}

	for _, tc := range tests {
		var cfg IndexdConfig
		err := yaml.Unmarshal([]byte(tc.yaml), &cfg)

		if tc.fail {
			if err == nil {
				t.Fatalf("%q: want an error, got %v", tc.yaml, cfg.MaxBufferAge)
			}
			continue
		}

		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if cfg.MaxBufferAge != tc.want {
			t.Fatalf("%q: want %v, got %v", tc.yaml, tc.want, cfg.MaxBufferAge)
		}
	}
}

// TestIndexdConfigRoundTrip verifies that the packing settings survive being
// written out and read back, and that the defaults stay out of the file.
func TestIndexdConfigRoundTrip(t *testing.T) {
	cfg := IndexdConfig{
		Name:              "Sombrero",
		MinPackedSlabSize: 1 << 20,
		MaxBufferAge:      BufferAge(24 * time.Hour),
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got IndexdConfig
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal(%q): %v", out, err)
	}
	if got != cfg {
		t.Fatalf("want %+v, got %+v", cfg, got)
	}

	// Left at their defaults, neither setting clutters the config file.
	out, err = yaml.Marshal(IndexdConfig{Name: "Sombrero"})
	if err != nil {
		t.Fatalf("Marshal defaults: %v", err)
	}
	for _, key := range []string{"minPackedSlabSize", "maxBufferAge"} {
		if strings.Contains(string(out), key) {
			t.Fatalf("want %q left out of a default config, got %q", key, out)
		}
	}
}

// TestReadConfigRejectsUnknownFields verifies that a mistyped setting is
// reported instead of being silently ignored, which is what keeps a typo in the
// packing settings from leaving them at their defaults unnoticed.
func TestReadConfigRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "sombrero.yml"), []byte(body), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	write("indexd:\n  maxBufferAge: 24h\n")
	if _, err := ReadConfig(dir); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	write("indexd:\n  maxBufferAg: 24h\n")
	if _, err := ReadConfig(dir); err == nil {
		t.Fatal("ReadConfig with a mistyped setting: want an error, got none")
	}

	if _, err := ReadConfig(t.TempDir()); err == nil {
		t.Fatal("ReadConfig without a config file: want an error, got none")
	}
}

// TestReadConfigDefaultsAPIAddress verifies that a config that says nothing
// about the API address gets localhost rather than every interface, since
// net.Listen treats an empty address as a wildcard bind on a random port.
func TestReadConfigDefaultsAPIAddress(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "sombrero.yml"), []byte(body), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	write("api:\n  password: hunter2\n")
	cfg, err := ReadConfig(dir)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if cfg.API.Address != defaultAPIAddress {
		t.Fatalf("want the API address defaulted to %q, got %q", defaultAPIAddress, cfg.API.Address)
	}

	// An address that is spelled out is left alone.
	write("api:\n  address: 0.0.0.0:8080\n  password: hunter2\n")
	cfg, err = ReadConfig(dir)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if cfg.API.Address != "0.0.0.0:8080" {
		t.Fatalf("want the configured API address kept, got %q", cfg.API.Address)
	}
}

// TestSaveConfigRoundTrip verifies that a saved config reads back as it was,
// since the server rewrites the file after generating a seed phrase.
func TestSaveConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cfg := Config{
		Debug:          true,
		Mode:           ModeLite,
		MaxConnections: 30,
		API:            APIConfig{Address: "127.0.0.1:9999", Password: "hunter2"},
		Database:       DatabaseConfig{Host: "127.0.0.1", Port: 5432, User: "postgres", Database: "sombrero", SSLMode: "disable"},
		Indexd: IndexdConfig{
			Name:              "Sombrero",
			SeedPhrase:        "seed",
			MinPackedSlabSize: 1 << 20,
			MaxBufferAge:      BufferAge(24 * time.Hour),
		},
	}

	if err := SaveConfig(cfg, dir); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got, err := ReadConfig(dir)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if got != cfg {
		t.Fatalf("want %+v, got %+v", cfg, got)
	}
}
