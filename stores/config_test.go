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

// TestCheckIntervalYAML verifies how the interval of a periodic check is read
// from the config file, in particular that leaving it out means the default
// while turning the check off has to be said out loud.
func TestCheckIntervalYAML(t *testing.T) {
	tests := []struct {
		yaml string
		want CheckInterval
		fail bool
	}{
		{yaml: "fragmentationCheck: 6h", want: CheckInterval(6 * time.Hour)},
		{yaml: "fragmentationCheck: 30m", want: CheckInterval(30 * time.Minute)},
		{yaml: "fragmentationCheck: never", want: -1},
		{yaml: "fragmentationCheck: Never", want: -1},
		{yaml: "fragmentationCheck: off", want: -1},
		{yaml: "fragmentationCheck: default", want: 0},
		{yaml: "fragmentationCheck: ''", want: 0},
		{yaml: "fragmentationCheck: '  6h  '", want: CheckInterval(6 * time.Hour)},
		{yaml: "appName: Sombrero", want: 0},
		{yaml: "fragmentationCheck: 0", fail: true},
		{yaml: "fragmentationCheck: 0s", fail: true},
		{yaml: "fragmentationCheck: 3600", fail: true},
		{yaml: "fragmentationCheck: -1h", fail: true},
		{yaml: "fragmentationCheck: sometimes", fail: true},
	}

	for _, tc := range tests {
		var cfg IndexdConfig
		err := yaml.Unmarshal([]byte(tc.yaml), &cfg)

		if tc.fail {
			if err == nil {
				t.Fatalf("%q: want an error, got %v", tc.yaml, cfg.FragmentationCheck)
			}
			continue
		}

		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if cfg.FragmentationCheck != tc.want {
			t.Fatalf("%q: want %v, got %v", tc.yaml, tc.want, cfg.FragmentationCheck)
		}
	}
}

// TestFragmentationDefaults verifies that the monitor's settings fall back to
// the defaults when the config leaves them out, and that turning the check off
// is reported as an interval of zero.
func TestFragmentationDefaults(t *testing.T) {
	tests := []struct {
		cfg            IndexdConfig
		wantThreshold  float64
		wantInterval   time.Duration
		wantDefragment bool
	}{
		{
			cfg:           IndexdConfig{},
			wantThreshold: DefaultFragmentationThreshold,
			wantInterval:  DefaultFragmentationCheck,
		},
		{
			cfg:           IndexdConfig{FragmentationThreshold: 0.5, FragmentationCheck: CheckInterval(6 * time.Hour)},
			wantThreshold: 0.5,
			wantInterval:  6 * time.Hour,
		},
		{
			// Off leaves the threshold alone: it still applies to the
			// listing the API serves on demand.
			cfg:           IndexdConfig{FragmentationThreshold: 0.5, FragmentationCheck: -1},
			wantThreshold: 0.5,
			wantInterval:  0,
		},
		{
			// Repacking is opt-in, and does not come with a default of
			// its own: it runs at whatever the check is set to.
			cfg:            IndexdConfig{Defragment: true},
			wantThreshold:  DefaultFragmentationThreshold,
			wantInterval:   DefaultFragmentationCheck,
			wantDefragment: true,
		},
	}

	for _, tc := range tests {
		threshold, interval, defragment := tc.cfg.Fragmentation()
		if threshold != tc.wantThreshold || interval != tc.wantInterval || defragment != tc.wantDefragment {
			t.Fatalf("%+v: want %v every %v (defragment %v), got %v every %v (defragment %v)",
				tc.cfg, tc.wantThreshold, tc.wantInterval, tc.wantDefragment, threshold, interval, defragment)
		}
	}
}

// TestReadConfigRejectsBadThreshold verifies that a threshold that would report
// either every slab or none of them is refused rather than quietly applied.
func TestReadConfigRejectsBadThreshold(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "sombrero.yml"), []byte(body), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	for _, body := range []string{"indexd:\n  fragmentationThreshold: -0.1\n", "indexd:\n  fragmentationThreshold: 1.5\n"} {
		write(body)
		if _, err := ReadConfig(dir); err == nil {
			t.Fatalf("ReadConfig(%q): want an error, got none", body)
		}
	}

	write("indexd:\n  fragmentationThreshold: 0.25\n")
	if _, err := ReadConfig(dir); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
}

// TestIndexdConfigRoundTrip verifies that the packing settings survive being
// written out and read back, and that the defaults stay out of the file.
func TestIndexdConfigRoundTrip(t *testing.T) {
	cfg := IndexdConfig{
		Name:                   "Sombrero",
		MinPackedSlabSize:      1 << 20,
		MaxBufferAge:           BufferAge(24 * time.Hour),
		FragmentationThreshold: 0.4,
		FragmentationCheck:     CheckInterval(6 * time.Hour),
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
	for _, key := range []string{"minPackedSlabSize", "maxBufferAge", "fragmentationThreshold", "fragmentationCheck"} {
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
			Name:                   "Sombrero",
			SeedPhrase:             "seed",
			MinPackedSlabSize:      1 << 20,
			MaxBufferAge:           BufferAge(24 * time.Hour),
			FragmentationThreshold: 0.4,
			FragmentationCheck:     CheckInterval(6 * time.Hour),
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
