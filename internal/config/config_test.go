package config

import (
	"strings"
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_URL", "postgres://localhost/test")
}

func TestHoldSweeperIsOffByDefault(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("HOLD_SWEEPER_ENABLED", "")
	t.Setenv("HOLD_SWEEP_INTERVAL", "")

	cfg, err := Load()

	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.HoldSweeperEnabled {
		t.Error("HoldSweeperEnabled = true, want false until OMS has agreed the expiry rule")
	}
	if cfg.HoldSweepInterval != time.Minute {
		t.Errorf("HoldSweepInterval = %v, want 1m", cfg.HoldSweepInterval)
	}
}

func TestHoldSweeperSettingsAreRead(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("HOLD_SWEEPER_ENABLED", "true")
	t.Setenv("HOLD_SWEEP_INTERVAL", "30s")

	cfg, err := Load()

	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !cfg.HoldSweeperEnabled || cfg.HoldSweepInterval != 30*time.Second {
		t.Errorf("settings = %v, %v, want true, 30s", cfg.HoldSweeperEnabled, cfg.HoldSweepInterval)
	}
}

func TestInvalidHoldSweeperSettingsFailStartup(t *testing.T) {
	tests := []struct {
		name, key, value string
	}{
		{"not a bool", "HOLD_SWEEPER_ENABLED", "maybe"},
		{"not a duration", "HOLD_SWEEP_INTERVAL", "soon"},
		{"zero interval", "HOLD_SWEEP_INTERVAL", "0s"},
		{"negative interval", "HOLD_SWEEP_INTERVAL", "-1m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()

			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Errorf("Load() error = %v, want one naming %s", err, tt.key)
			}
		})
	}
}
