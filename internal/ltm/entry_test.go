package ltm

import (
	"testing"
	"time"
)

func TestSignatureNormalizes(t *testing.T) {
	a := Signature("  Hello   World  ")
	b := Signature("hello world")
	if a != b {
		t.Fatalf("规范化后签名应相等: %s != %s", a, b)
	}
	if Signature("hello world") == Signature("goodbye world") {
		t.Fatal("不同内容签名不应相等")
	}
}

func TestEntryExpired(t *testing.T) {
	now := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		ttlDays int
		updated time.Time
		want    bool
	}{
		{"永不过期", 0, now.Add(-100 * 24 * time.Hour), false},
		{"未过期", 30, now.Add(-10 * 24 * time.Hour), false},
		{"已过期", 30, now.Add(-31 * 24 * time.Hour), true},
		{"恰好到期", 30, now.Add(-30 * 24 * time.Hour), false},
		{"负 TTL 永不过期", -1, now.Add(-100 * 24 * time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Entry{TTLDays: tt.ttlDays, UpdatedAt: tt.updated}
			if got := e.Expired(now); got != tt.want {
				t.Errorf("Expired()=%v want %v", got, tt.want)
			}
		})
	}
}
