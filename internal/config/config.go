// Package config loads the service configuration from environment variables.
package config

import (
	"errors"
	"strings"
)

// Config is the validated service configuration.
type Config struct {
	ListenAddr  string
	DatabaseURL string
	// AuthIssuer is the auth service's public URL, as it appears in iss.
	AuthIssuer string
	// AuthURL is where this service reaches it. It defaults to AuthIssuer.
	AuthURL string
	// ClientID is this service's account name at the auth service.
	ClientID     string
	ClientSecret string
	// OTLPEndpoint is an OTLP/gRPC host:port. Empty turns telemetry off.
	OTLPEndpoint string
}

// Load reads and validates the configuration through getenv.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:   getenv("COLLAB_LISTEN_ADDR"),
		DatabaseURL:  getenv("DATABASE_URL"),
		AuthIssuer:   strings.TrimRight(getenv("AUTH_ISSUER"), "/"),
		AuthURL:      strings.TrimRight(getenv("AUTH_URL"), "/"),
		ClientID:     getenv("AUTH_CLIENT_ID"),
		ClientSecret: getenv("AUTH_CLIENT_SECRET"),
		OTLPEndpoint: getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8082"
	}
	if cfg.AuthURL == "" {
		cfg.AuthURL = cfg.AuthIssuer
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "t10-collab"
	}
	switch {
	case cfg.DatabaseURL == "":
		return Config{}, errors.New("DATABASE_URL is required")
	case cfg.AuthIssuer == "":
		return Config{}, errors.New("AUTH_ISSUER is required")
	case cfg.ClientSecret == "":
		return Config{}, errors.New("AUTH_CLIENT_SECRET is required")
	}
	return cfg, nil
}
