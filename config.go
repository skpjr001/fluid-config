package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// StaticConfig is read once at startup. Everything else lives in apis_dir.
type StaticConfig struct {
	Addr              string `json:"addr"`
	APIsDir           string `json:"apis_dir"`
	BasePath          string `json:"base_path"` // e.g. "/api" -> /api/v1/holidays
	ReloadInterval    string `json:"reload_interval"`
	TrustForwardedFor bool   `json:"trust_forwarded_for"` // true only behind Traefik/Nginx
	CatalogToken      string `json:"catalog_token"`       // empty = /_catalog and /openapi.json are public
	MaxBodyBytes      int64  `json:"max_body_bytes"`
	MaxPageSize       int    `json:"max_page_size"`

	reloadEvery time.Duration
}

func loadStatic(path string) (*StaticConfig, error) {
	c := &StaticConfig{
		Addr:           ":8090",
		APIsDir:        "./apis",
		ReloadInterval: "2s",
		MaxBodyBytes:   1 << 20,
		MaxPageSize:    1000,
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if v := os.Getenv("FLUID_CATALOG_TOKEN"); v != "" {
		c.CatalogToken = v
	}
	c.BasePath = "/" + strings.Trim(c.BasePath, "/")
	if c.BasePath == "/" {
		c.BasePath = ""
	}
	d, err := time.ParseDuration(c.ReloadInterval)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("bad reload_interval %q", c.ReloadInterval)
	}
	c.reloadEvery = d
	return c, nil
}
