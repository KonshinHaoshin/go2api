package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration loaded from YAML.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Cache    CacheConfig    `yaml:"cache"`
	KeyPool  KeyPoolConfig  `yaml:"keypool"`
	Log      LogConfig      `yaml:"log"`
}

type ServerConfig struct {
	Listen      string   `yaml:"listen"`
	AuthTokens  []string `yaml:"auth_tokens"`
	ReadTimeout int      `yaml:"read_timeout_seconds"`
}

type UpstreamConfig struct {
	BaseURL    string        `yaml:"base_url"`
	Timeout    time.Duration `yaml:"-"`
	TimeoutSec int           `yaml:"timeout_seconds"`
}

type CacheConfig struct {
	Enabled       bool          `yaml:"enabled"`
	TTL           time.Duration `yaml:"-"`
	TTLSeconds    int           `yaml:"ttl_seconds"`
	SkipStreaming bool          `yaml:"skip_streaming"`
	MaxBytes      int64         `yaml:"max_response_bytes"`
}

type KeyPoolConfig struct {
	Strategy string         `yaml:"strategy"` // round_robin | weighted | quota_aware
	Failover FailoverConfig `yaml:"failover"`
	Keys     []KeyConfig    `yaml:"keys"`
}

type FailoverConfig struct {
	Enabled    bool  `yaml:"enabled"`
	MaxRetries int   `yaml:"max_retries"`
	RetryOn    []int `yaml:"retry_on"` // HTTP status codes
	CoolDown   int   `yaml:"cooldown_seconds"`
}

type KeyConfig struct {
	ID       string `yaml:"id"` // optional; auto-generated if empty
	Label    string `yaml:"label"`
	APIKey   string `yaml:"api_key"`
	Weight   int    `yaml:"weight"`
	Disabled bool   `yaml:"disabled"`
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Pretty bool   `yaml:"pretty"` // human-readable vs JSON
}

// Load reads and parses the config file at the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 60
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://opencode.ai/zen/go/v1"
	}
	if c.Upstream.TimeoutSec == 0 {
		c.Upstream.TimeoutSec = 300
	}
	c.Upstream.Timeout = time.Duration(c.Upstream.TimeoutSec) * time.Second

	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 3600
	}
	c.Cache.TTL = time.Duration(c.Cache.TTLSeconds) * time.Second
	if !c.Cache.SkipStreaming {
		c.Cache.SkipStreaming = true
	}
	if c.Cache.MaxBytes == 0 {
		c.Cache.MaxBytes = 4 * 1024 * 1024 // 4 MiB
	}

	if c.KeyPool.Strategy == "" {
		c.KeyPool.Strategy = "round_robin"
	}
	if c.KeyPool.Failover.MaxRetries == 0 {
		c.KeyPool.Failover.MaxRetries = 2
	}
	if len(c.KeyPool.Failover.RetryOn) == 0 {
		c.KeyPool.Failover.RetryOn = []int{429, 500, 502, 503, 504}
	}
	if c.KeyPool.Failover.CoolDown == 0 {
		c.KeyPool.Failover.CoolDown = 60
	}
	for i := range c.KeyPool.Keys {
		if c.KeyPool.Keys[i].Weight <= 0 {
			c.KeyPool.Keys[i].Weight = 1
		}
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}

func (c *Config) validate() error {
	for i, k := range c.KeyPool.Keys {
		if k.APIKey == "" {
			return fmt.Errorf("keypool.keys[%d].api_key is required", i)
		}
		if k.Label == "" {
			return fmt.Errorf("keypool.keys[%d].label is required", i)
		}
	}
	switch c.KeyPool.Strategy {
	case "round_robin", "weighted", "quota_aware":
	default:
		return fmt.Errorf("keypool.strategy must be one of round_robin|weighted|quota_aware, got %q", c.KeyPool.Strategy)
	}
	return nil
}
