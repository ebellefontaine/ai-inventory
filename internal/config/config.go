package config

import (
	"fmt"
	"strconv"
)

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

	// These three are the operator's only levers over fleet behaviour, and
	// heartbeat.go's contract is that the interval can be changed without
	// redeploying agents -- the server returns it in every register response.
	// That promise is empty unless the interval is settable here.
	for _, f := range []struct {
		key string
		dst *int
	}{
		{"AIINV_HEARTBEAT_INTERVAL_SECONDS", &cfg.HeartbeatIntervalSeconds},
		{"AIINV_IDLE_AFTER_MISSED_BEATS", &cfg.IdleAfterMissedBeats},
		{"AIINV_STALE_AFTER_DAYS", &cfg.StaleAfterDays},
	} {
		v, ok := lookup(f.key)
		if !ok || v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %q is not a number", f.key, v)
		}
		// Refuse rather than clamp. Zero or negative would turn the lifecycle
		// sweep into a busy loop that marks live agents stale, and an operator
		// who typed it wants to hear about it now, not discover it later.
		if n < 1 {
			return Config{}, fmt.Errorf("%s must be at least 1, got %d", f.key, n)
		}
		*f.dst = n
	}

	return cfg, nil
}
