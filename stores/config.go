package stores

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// defaultAPIAddress is where the API listens when the config does not say.
const defaultAPIAddress = "127.0.0.1:9999"

// APIConfig lists the API-related fields.
type APIConfig struct {
	// Address is the address the API and the web UI listen on. It defaults
	// to localhost: the API administers the whole server, so it is not
	// exposed to the network unless it is asked for explicitly.
	Address  string `yaml:"address"`
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

// BufferAge is how long the data that does not fill a slab may sit in the
// database before it is uploaded anyway. The zero value means never: the data
// waits for as long as it takes for enough of it to accumulate, which keeps a
// partly filled slab from being paid for at the price of a full one.
type BufferAge time.Duration

// String implements fmt.Stringer.
func (a BufferAge) String() string {
	if a <= 0 {
		return "never"
	}
	return time.Duration(a).String()
}

// Duration returns the age as a time.Duration.
func (a BufferAge) Duration() time.Duration {
	return time.Duration(a)
}

// MarshalYAML implements yaml.Marshaler.
func (a BufferAge) MarshalYAML() (any, error) {
	return a.String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *BufferAge) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}

	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "never":
		*a = 0
		return nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("unknown buffer age: %q", s)
	}
	if d < 0 {
		return fmt.Errorf("buffer age must not be negative: %q", s)
	}

	// A zero duration is the zero value, i.e. never.
	*a = BufferAge(d)
	return nil
}

// IndexdConfig lists all parameters required to connect to an `indexd` node.
type IndexdConfig struct {
	Name        string `yaml:"appName"`
	Description string `yaml:"description"`
	LogoURL     string `yaml:"logoURL"`
	ServiceURL  string `yaml:"serviceURL"`
	SeedPhrase  string `yaml:"seedPhrase"`

	// The data of a file that does not fill a slab is kept in the database
	// until it can be packed into a full slab together with the data of other
	// files. These two set the point at which an incomplete slab is uploaded
	// regardless: once the leftover data of a share has been waiting for
	// MaxBufferAge and amounts to at least MinPackedSlabSize bytes.
	//
	// Unset, MaxBufferAge keeps the leftover data waiting indefinitely, so
	// that only full slabs are ever uploaded. Unset, MinPackedSlabSize puts
	// no lower bound on what an aged upload may carry.
	MinPackedSlabSize uint64    `yaml:"minPackedSlabSize,omitempty"`
	MaxBufferAge      BufferAge `yaml:"maxBufferAge,omitempty"`
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

	if err = dec.Decode(&cfg); err != nil {
		return
	}

	// An unset address must not fall back to binding every interface.
	if cfg.API.Address == "" {
		cfg.API.Address = defaultAPIAddress
	}

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
