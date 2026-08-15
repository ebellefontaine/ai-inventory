package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(func(string) (string, bool) { return "", false })
	require.ErrorContains(t, err, "AIINV_DATABASE_URL")
}

func TestLoadAppliesDefaults(t *testing.T) {
	env := map[string]string{"AIINV_DATABASE_URL": "postgres://x"}
	cfg, err := Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.ListenAddr)
	require.Equal(t, 60, cfg.HeartbeatIntervalSeconds)
}
