//go:build darwin && cgo

package quota

import (
	"testing"
	"time"
)

func TestKeychainDeniedCooldown(t *testing.T) {
	service := "test-service-cooldown"
	clearKeychainDenied(service)

	now := time.Now()
	if isKeychainDeniedCooldown(service, now) {
		t.Fatal("expected no cooldown initially")
	}

	recordKeychainDenied(service, now)

	// Within cooldown window:
	if !isKeychainDeniedCooldown(service, now.Add(1*time.Minute)) {
		t.Fatal("expected cooldown within 1 minute")
	}
	if !isKeychainDeniedCooldown(service, now.Add(4*time.Minute+59*time.Second)) {
		t.Fatal("expected cooldown before 5 minutes")
	}

	// Past cooldown window:
	if isKeychainDeniedCooldown(service, now.Add(5*time.Minute+1*time.Second)) {
		t.Fatal("expected cooldown expired past 5 minutes")
	}

	// Clearing cooldown:
	clearKeychainDenied(service)
	if isKeychainDeniedCooldown(service, now.Add(1*time.Minute)) {
		t.Fatal("expected cooldown cleared")
	}
}
