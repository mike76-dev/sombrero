package stores

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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
	MaxConnections int            `yaml:"maxConnections"`
	API            APIConfig      `yaml:"api"`
	Database       DatabaseConfig `yaml:"database"`
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
