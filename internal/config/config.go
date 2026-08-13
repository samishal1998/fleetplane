// Package config loads and validates the Fleetplane configuration file
// (ADR-007): strict YAML — unknown fields are boot errors — with all
// cross-references validated up front so the process fails fast.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the root of the configuration file. Fields are added
// increment-by-increment; strict decoding means the file may only contain
// what the running binary actually implements.
type Config struct {
	Server    Server              `yaml:"server"`
	Storage   Storage             `yaml:"storage"`
	Providers map[string]Provider `yaml:"providers"`
	Classes   map[string]Class    `yaml:"classes"`
	Engine    Engine              `yaml:"engine"`
	Reconcile Reconcile           `yaml:"reconcile"`
	Acquire   Acquire             `yaml:"acquire"`
}

// Class is a reusable creation template (04 §2). Spec may contain
// provider-specific fields; reclaim.idleAfter enables poolless idle
// reclamation for resources created from this class (plan R21).
type Class struct {
	Kind     string         `yaml:"kind"`
	Provider string         `yaml:"provider"`
	Spec     map[string]any `yaml:"spec"`
	Reclaim  *ClassReclaim  `yaml:"reclaim"`
}

type ClassReclaim struct {
	IdleAfter Duration `yaml:"idleAfter"`
}

// Reconcile tunes the pool reconciler.
type Reconcile struct {
	Interval             Duration `yaml:"interval"`
	MaxMutationsPerCycle int      `yaml:"maxMutationsPerCycle"` // R22: the one budget knob
}

// Acquire tunes acquisition handling.
type Acquire struct {
	// PendingTimeout expires never-satisfied acquisitions (plan R6).
	PendingTimeout Duration `yaml:"pendingTimeout"`
}

// Engine tunes the operation engine (ADR-014 defaults apply when zero).
type Engine struct {
	PollInterval Duration `yaml:"pollInterval"`
	VerifyWindow Duration `yaml:"verifyWindow"` // uncertain-create resolution window (plan R10)
}

// Provider configures one provider instance (03 §4). Settings is the raw
// driver config block; credentials inside must be secret:// references.
type Provider struct {
	Driver   string         `yaml:"driver"`
	Settings map[string]any `yaml:"settings"`
}

type Server struct {
	// Addr is the main API listen address.
	Addr string `yaml:"addr"`
	// OpsAddr serves /metrics, pprof and admin endpoints; keep it
	// loopback-only unless the network is trusted.
	OpsAddr string `yaml:"opsAddr"`
	// ShutdownGrace bounds graceful shutdown (07 §9).
	ShutdownGrace Duration `yaml:"shutdownGrace"`
}

type Storage struct {
	// Path is the SQLite database file path.
	Path string `yaml:"path"`
}

// Duration is a time.Duration that unmarshals from YAML strings like "20s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads, strictly decodes, defaults and validates the config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse is Load for in-memory bytes (used by tests).
func Parse(b []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalWithOptions(b, &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("config: %s", yaml.FormatError(err, false, true))
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.OpsAddr == "" {
		c.Server.OpsAddr = "127.0.0.1:9090"
	}
	if c.Server.ShutdownGrace == 0 {
		c.Server.ShutdownGrace = Duration(20 * time.Second)
	}
}

func (c *Config) validate() error {
	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}
	for name, p := range c.Providers {
		if p.Driver == "" {
			return fmt.Errorf("providers.%s.driver is required", name)
		}
	}
	for name, cls := range c.Classes {
		if cls.Kind == "" || cls.Provider == "" {
			return fmt.Errorf("classes.%s needs kind and provider", name)
		}
		if _, ok := c.Providers[cls.Provider]; !ok {
			return fmt.Errorf("classes.%s references unknown provider %q", name, cls.Provider)
		}
	}
	return nil
}
