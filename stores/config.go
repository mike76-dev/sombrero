package stores

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServerMode describes the mode in which the server is running.
type ServerMode int

const (
	ModeNormal ServerMode = iota
	ModeLite
)

// String implements fmt.Stringer.
func (m ServerMode) String() string {
	switch m {
	case ModeNormal:
		return "normal"
	case ModeLite:
		return "lite"
	default:
		return "unknown"
	}
}

// MarshalYAML implements yaml.Marshaler.
func (m ServerMode) MarshalYAML() (any, error) {
	if m != ModeNormal && m != ModeLite {
		return nil, fmt.Errorf("unknown server mode: %d", m)
	}
	return m.String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (m *ServerMode) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	switch strings.ToLower(s) {
	case "", "normal":
		*m = ModeNormal
	case "lite":
		*m = ModeLite
	default:
		return fmt.Errorf("unknown server mode: %q", s)
	}
	return nil
}

// APIConfig lists the API-related fields.
type APIConfig struct {
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
}

// DatabaseConfig lists all the fields needed to connect to a PostgreSQL database.
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"sslMode"`
}

// String returns a connection string.
func (dc DatabaseConfig) String() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s", dc.Host, dc.Port, dc.User, dc.Password, dc.Database, dc.SSLMode)
}

// IndexdConfig lists all parameters required to connect to an `indexd` node.
type IndexdConfig struct {
	Name        string `yaml:"appName"`
	Description string `yaml:"description"`
	LogoURL     string `yaml:"logoURL"`
	ServiceURL  string `yaml:"serviceURL"`
	SeedPhrase  string `yaml:"seedPhrase"`
}

// Config lists the config fields.
type Config struct {
	Debug          bool           `yaml:"debug"`
	Mode           ServerMode     `yaml:"mode"`
	MaxConnections int            `yaml:"maxConnections"`
	API            APIConfig      `yaml:"api"`
	Database       DatabaseConfig `yaml:"database,omitempty"`
	Indexd         IndexdConfig   `yaml:"indexd,omitempty"`
}

// ReadConfig tries to read the config from the specified directory.
func ReadConfig(dir string) (cfg Config, err error) {
	path := filepath.Join(dir, "sombrero.yml")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	err = dec.Decode(&cfg)
	return
}

// SaveConfig saves the config to the specified directory.
func SaveConfig(cfg Config, dir string) error {
	path := filepath.Join(dir, "sombrero.yml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("failed to encode config file: %v", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %v", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close file: %v", err)
	}

	return nil
}
