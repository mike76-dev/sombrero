package stores

import (
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
		{yaml: "appName: Sombrero", want: 0},
		{yaml: "maxBufferAge: 0s", fail: true},
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
