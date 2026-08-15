package config

import "fmt"

type Config struct {
	DatabaseURL              string
	ListenAddr               string
	LogLevel                 string
	HeartbeatIntervalSeconds int
	IdleAfterMissedBeats     int
	StaleAfterDays           int
}

type LookupFunc func(string) (string, bool)

func Load(lookup LookupFunc) (Config, error) {
	cfg := Config{
		ListenAddr:               ":8080",
		LogLevel:                 "info",
		HeartbeatIntervalSeconds: 60,
		IdleAfterMissedBeats:     3,
		StaleAfterDays:           7,
	}

	url, ok := lookup("AIINV_DATABASE_URL")
	if !ok || url == "" {
		return Config{}, fmt.Errorf("AIINV_DATABASE_URL is required")
	}
	cfg.DatabaseURL = url

	if v, ok := lookup("AIINV_LISTEN_ADDR"); ok && v != "" {
		cfg.ListenAddr = v
	}
	if v, ok := lookup("AIINV_LOG_LEVEL"); ok && v != "" {
		cfg.LogLevel = v
	}
	return cfg, nil
}
