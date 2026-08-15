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
	Discovery Discovery           `yaml:"discovery"`
	Auth      Auth                `yaml:"auth"`
}

// Auth holds static API tokens (07 §2, ADR-008). With no tokens configured
// the API runs OPEN — local development only; boot logs a loud warning.
type Auth struct {
	Tokens []Token `yaml:"tokens"`
}

type Token struct {
	ID          string   `yaml:"id"`     // 8-hex lookup prefix
	Name        string   `yaml:"name"`   // shown in audit events
	SHA256      string   `yaml:"sha256"` // hex of sha256(secret)
	Permissions []string `yaml:"permissions"`
}

// Class is a reusable creation template (04 §2). Spec may contain
// provider-specific fields; reclaim.idleAfter enables poolless idle
// reclamation for resources created from this class (plan R21).
type Class struct {
	Kind       string           `yaml:"kind"`
	Provider   string           `yaml:"provider"`
	Spec       map[string]any   `yaml:"spec"`
	Reclaim    *ClassReclaim    `yaml:"reclaim"`
	Scheduling *ClassScheduling `yaml:"scheduling"`
}

// ClassScheduling holds the class's cost/latency tradeoff (docs/11 §8).
type ClassScheduling struct {
	Queue *QueuePolicy `yaml:"queue"`
}

// QueuePolicy: maxWait > 0 lets acquisitions of this class wait for
// existing or in-flight capacity before scaling up (sequential lease
// packing); 0/absent = provision immediately (today's behavior).
type QueuePolicy struct {
	MaxWait Duration `yaml:"maxWait"`
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

// Discovery tunes the provider discovery sweep (ADR-017).
type Discovery struct {
	Interval    Duration `yaml:"interval"`
	OrphanGrace Duration `yaml:"orphanGrace"`
	// GhostPolicy: delete (default) | surface.
	GhostPolicy string `yaml:"ghostPolicy"`
	// AdoptUnlabeled: off (default) | observed.
	AdoptUnlabeled string `yaml:"adoptUnlabeled"`
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
	// Billing overrides the driver's billing capability per resource kind
	// (docs/11 §3–4). Fields are pointers: unset fields keep the driver's
	// value, so a partial override never silently zeroes the rest.
	Billing map[string]BillingOverride `yaml:"billing"`
}

// BillingOverride tunes one kind's billing policy (docs/11). disabled: true
// is the explicit opt-out (zero policy = fine-grained = no billing-window
// behavior); it may not be combined with the other fields.
type BillingOverride struct {
	Disabled          bool      `yaml:"disabled"`
	MinimumDuration   *Duration `yaml:"minimumDuration"`
	Increment         *Duration `yaml:"increment"`
	TerminationBuffer *Duration `yaml:"terminationBuffer"`
	// Adaptive derives the termination buffer from observed provider
	// deletion durations (p95 + margin, floored by terminationBuffer,
	// capped at increment/2 — docs/11 §12).
	Adaptive bool `yaml:"adaptive"`
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
	// One effective value everywhere (plan R6 default; the queue clamp and
	// the boot sweep both read this — never a scattered fallback).
	if c.Acquire.PendingTimeout == 0 {
		c.Acquire.PendingTimeout = Duration(15 * time.Minute)
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
		for kind, b := range p.Billing {
			if b.Disabled && (b.MinimumDuration != nil || b.Increment != nil || b.TerminationBuffer != nil || b.Adaptive) {
				return fmt.Errorf("providers.%s.billing.%s: disabled may not be combined with other fields", name, kind)
			}
			for fname, d := range map[string]*Duration{
				"minimumDuration": b.MinimumDuration, "increment": b.Increment, "terminationBuffer": b.TerminationBuffer,
			} {
				if d != nil && *d < 0 {
					return fmt.Errorf("providers.%s.billing.%s.%s must be >= 0", name, kind, fname)
				}
			}
		}
	}
	for name, cls := range c.Classes {
		if cls.Kind == "" || cls.Provider == "" {
			return fmt.Errorf("classes.%s needs kind and provider", name)
		}
		if _, ok := c.Providers[cls.Provider]; !ok {
			return fmt.Errorf("classes.%s references unknown provider %q", name, cls.Provider)
		}
		if cls.Scheduling != nil && cls.Scheduling.Queue != nil {
			mw := cls.Scheduling.Queue.MaxWait
			if mw < 0 {
				return fmt.Errorf("classes.%s.scheduling.queue.maxWait must be >= 0", name)
			}
			if mw.Std() > c.Acquire.PendingTimeout.Std() {
				return fmt.Errorf("classes.%s.scheduling.queue.maxWait (%s) exceeds acquire.pendingTimeout (%s)",
					name, mw.Std(), c.Acquire.PendingTimeout.Std())
			}
		}
	}
	seen := map[string]bool{}
	for i, tok := range c.Auth.Tokens {
		if tok.ID == "" || tok.SHA256 == "" {
			return fmt.Errorf("auth.tokens[%d] needs id and sha256", i)
		}
		if len(tok.SHA256) != 64 {
			return fmt.Errorf("auth.tokens[%d].sha256 must be 64 hex chars", i)
		}
		if seen[tok.ID] {
			return fmt.Errorf("auth.tokens: duplicate id %q", tok.ID)
		}
		seen[tok.ID] = true
	}
	return nil
}
