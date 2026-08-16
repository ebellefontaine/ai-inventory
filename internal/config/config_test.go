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
	require.Equal(t, 3, cfg.IdleAfterMissedBeats)
	require.Equal(t, 7, cfg.StaleAfterDays)
}

func lookupFrom(env map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// The lifecycle settings are the operator's only levers over fleet behaviour.
// Until this test existed they were defaults that no environment could reach.
func TestLoadReadsLifecycleSettingsFromEnvironment(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"AIINV_DATABASE_URL":               "postgres://x",
		"AIINV_HEARTBEAT_INTERVAL_SECONDS": "15",
		"AIINV_IDLE_AFTER_MISSED_BEATS":    "5",
		"AIINV_STALE_AFTER_DAYS":           "30",
	}))
	require.NoError(t, err)
	require.Equal(t, 15, cfg.HeartbeatIntervalSeconds)
	require.Equal(t, 5, cfg.IdleAfterMissedBeats)
	require.Equal(t, 30, cfg.StaleAfterDays)
}

func TestLoadRejectsUnusableLifecycleSettings(t *testing.T) {
	for _, tc := range []struct{ name, key, value, want string }{
		{"not a number", "AIINV_HEARTBEAT_INTERVAL_SECONDS", "soon", "is not a number"},
		{"zero", "AIINV_HEARTBEAT_INTERVAL_SECONDS", "0", "must be at least 1"},
		{"negative", "AIINV_IDLE_AFTER_MISSED_BEATS", "-1", "must be at least 1"},
		{"zero days", "AIINV_STALE_AFTER_DAYS", "0", "must be at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{
				"AIINV_DATABASE_URL": "postgres://x",
				tc.key:               tc.value,
			}))
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, tc.key, "the error must name the offending variable")
		})
	}
}

// An empty value is an unset value, not a parse error -- shells routinely
// export empty strings and refusing to start over one would be hostile.
func TestLoadTreatsEmptyLifecycleValuesAsUnset(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"AIINV_DATABASE_URL":               "postgres://x",
		"AIINV_HEARTBEAT_INTERVAL_SECONDS": "",
	}))
	require.NoError(t, err)
	require.Equal(t, 60, cfg.HeartbeatIntervalSeconds)
}
