package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type TargetConfig struct {
	Name               string            `yaml:"name"`
	URL                string            `yaml:"url"`
	Username           string            `yaml:"username"`
	Password           string            `yaml:"password"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify"`
	Headers            map[string]string `yaml:"headers"`
	Timeout            Duration          `yaml:"timeout"`
}

type Config struct {
	Listen        string         `yaml:"listen"`
	ScrapeTimeout Duration       `yaml:"scrape_timeout"`
	CacheTTL      Duration       `yaml:"cache_ttl"`
	Targets       []TargetConfig `yaml:"targets"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{
		Listen:        ":9108",
		ScrapeTimeout: Duration(15 * time.Second),
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("no targets configured")
	}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.URL == "" {
			return nil, fmt.Errorf("targets[%d]: url is required", i)
		}
		if t.Name == "" {
			t.Name = t.URL
		}
		if t.Timeout == 0 {
			t.Timeout = cfg.ScrapeTimeout
		}
	}
	return cfg, nil
}
