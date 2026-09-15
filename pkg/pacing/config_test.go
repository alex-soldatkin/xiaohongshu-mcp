package pacing

import (
	"testing"
	"time"
)

func TestConfigFromEnv_Overrides(t *testing.T) {
	t.Setenv("XHS_MIN_GAP_MS", "1500")
	t.Setenv("XHS_MAX_READS_PER_HOUR", "0")
	t.Setenv("XHS_MAX_WRITES_PER_HOUR", "7")
	t.Setenv("XHS_MAX_WRITES_PER_DAY", "70")
	t.Setenv("XHS_MAX_PUBLISH_PER_DAY", "2")
	t.Setenv("XHS_RISK_COOLDOWN", "90s")
	t.Setenv("XHS_PACING_STATE", "/tmp/whatever.json")

	cfg := ConfigFromEnv()

	if cfg.MinGap != 1500*time.Millisecond {
		t.Errorf("MinGap = %s, want 1.5s", cfg.MinGap)
	}
	if cfg.MaxGap != 4*time.Second {
		t.Errorf("MaxGap = %s, want 4s", cfg.MaxGap)
	}
	if cfg.MaxReadsPerHour != 0 {
		t.Errorf("MaxReadsPerHour = %d, want 0 (explicit zero must disable the limit)", cfg.MaxReadsPerHour)
	}
	if cfg.MaxWritesPerHour != 7 || cfg.MaxWritesPerDay != 70 || cfg.MaxPublishPerDay != 2 {
		t.Errorf("unexpected write/publish budgets: %+v", cfg)
	}
	if cfg.RiskCooldown != 90*time.Second {
		t.Errorf("RiskCooldown = %s, want 90s", cfg.RiskCooldown)
	}
	if cfg.StatePath != "/tmp/whatever.json" {
		t.Errorf("StatePath = %s", cfg.StatePath)
	}
}

func TestConfigFromEnv_IgnoresGarbageAndDefaults(t *testing.T) {
	t.Setenv("XHS_MAX_WRITES_PER_HOUR", "not-a-number")
	t.Setenv("XHS_RISK_COOLDOWN", "1800") // bare seconds are accepted

	cfg := ConfigFromEnv()

	if cfg.MaxWritesPerHour != DefaultConfig().MaxWritesPerHour {
		t.Errorf("MaxWritesPerHour = %d, want the default", cfg.MaxWritesPerHour)
	}
	if cfg.RiskCooldown != 30*time.Minute {
		t.Errorf("RiskCooldown = %s, want 30m", cfg.RiskCooldown)
	}
}

func TestGapDistribution_StaysInsideItsWindow(t *testing.T) {
	sample := gapSampler(3*time.Second, 8*time.Second)

	for i := 0; i < 2000; i++ {
		d := sample()
		if d < 3*time.Second || d > 8*time.Second {
			t.Fatalf("sample %s outside [3s, 8s]", d)
		}
	}

	if got := gapSampler(0, 0)(); got != 0 {
		t.Fatalf("zero MinGap must disable the gap, got %s", got)
	}
}
