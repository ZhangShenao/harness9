package sandbox

import (
	"testing"
	"time"
)

func TestDefaultConfig_Defaults(t *testing.T) {
	t.Setenv("SANDBOX_IMAGE", "")
	t.Setenv("SANDBOX_CPUS", "")
	t.Setenv("SANDBOX_MEMORY", "")
	t.Setenv("SANDBOX_ENABLED", "")

	cfg := DefaultConfig()

	if !cfg.Enabled {
		t.Error("Enabled 默认应为 true")
	}
	if cfg.Image != "ubuntu:22.04" {
		t.Errorf("Image = %q, 期望 ubuntu:22.04", cfg.Image)
	}
	if cfg.CPUs != "1.0" {
		t.Errorf("CPUs = %q, 期望 1.0", cfg.CPUs)
	}
	if cfg.Memory != "512m" {
		t.Errorf("Memory = %q, 期望 512m", cfg.Memory)
	}
	if cfg.PidsLimit != 256 {
		t.Errorf("PidsLimit = %d, 期望 256", cfg.PidsLimit)
	}
	if cfg.StartTimeout != 60*time.Second {
		t.Errorf("StartTimeout = %v, 期望 60s", cfg.StartTimeout)
	}
	if cfg.StopTimeout != 10*time.Second {
		t.Errorf("StopTimeout = %v, 期望 10s", cfg.StopTimeout)
	}
}

func TestDefaultConfig_FromEnv(t *testing.T) {
	t.Setenv("SANDBOX_ENABLED", "true")
	t.Setenv("SANDBOX_IMAGE", "alpine:3.18")
	t.Setenv("SANDBOX_CPUS", "2.0")
	t.Setenv("SANDBOX_MEMORY", "256m")

	cfg := DefaultConfig()

	if !cfg.Enabled {
		t.Error("SANDBOX_ENABLED=true 时 Enabled 应为 true")
	}
	if cfg.Image != "alpine:3.18" {
		t.Errorf("Image = %q, 期望 alpine:3.18", cfg.Image)
	}
	if cfg.CPUs != "2.0" {
		t.Errorf("CPUs = %q, 期望 2.0", cfg.CPUs)
	}
	if cfg.Memory != "256m" {
		t.Errorf("Memory = %q, 期望 256m", cfg.Memory)
	}
}

func TestDefaultConfig_DisabledWhenFalse(t *testing.T) {
	cases := []string{"false", "False", "FALSE"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SANDBOX_ENABLED", v)
			if DefaultConfig().Enabled {
				t.Errorf("SANDBOX_ENABLED=%q 时 Enabled 应为 false", v)
			}
		})
	}
}

func TestDefaultConfig_EnabledForNonFalseValues(t *testing.T) {
	cases := []string{"0", "no", "1", "yes", "true"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SANDBOX_ENABLED", v)
			if !DefaultConfig().Enabled {
				t.Errorf("SANDBOX_ENABLED=%q 时 Enabled 应为 true（仅 'false' 关闭）", v)
			}
		})
	}
}
