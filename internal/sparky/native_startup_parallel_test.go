package sparky

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeStartupLocksAllowIndependentDevicesAndBlockSharedDevices(t *testing.T) {
	dir := t.TempDir()
	first, err := nativeStartupLocks(context.Background(), dir, []string{"spark01"})
	if err != nil {
		t.Fatal(err)
	}
	defer nativeCloseLocks(first)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	other, err := nativeStartupLocks(ctx, dir, []string{"spark02"})
	if err != nil {
		t.Fatalf("an independent device waited on the other device's startup: %v", err)
	}
	nativeCloseLocks(other)
	for _, nodes := range [][]string{{"spark01"}, {"spark02", "spark01"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		locks, err := nativeStartupLocks(ctx, dir, nodes)
		cancel()
		if err == nil {
			nativeCloseLocks(locks)
			t.Fatalf("overlapping startup did not wait for spark01: %v", nodes)
		}
	}
	// A cancelled overlap must leave the independent device usable.
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	locks, err := nativeStartupLocks(ctx, dir, []string{"spark02"})
	if err != nil {
		t.Fatalf("cancelled startup leaked a device lock: %v", err)
	}
	nativeCloseLocks(locks)
}

func TestNativeStartupLocksReleasePartialAcquisitions(t *testing.T) {
	dir := t.TempDir()
	second, err := nativeStartupLocks(context.Background(), dir, []string{"spark02"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	locks, err := nativeStartupLocks(ctx, dir, []string{"spark01", "spark02"})
	cancel()
	if err == nil {
		nativeCloseLocks(locks)
		t.Error("overlapping two-device acquisition should wait")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	first, err := nativeStartupLocks(ctx, dir, []string{"spark01"})
	cancel()
	if err != nil {
		t.Fatalf("failed acquisition retained its first device lock: %v", err)
	}
	nativeCloseLocks(first)
	nativeCloseLocks(second)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	legacy, err := nativeLockPaths(ctx, []string{filepath.Join(dir, "operation.lock")})
	if err != nil {
		t.Fatalf("failed acquisition retained its compatibility lock: %v", err)
	}
	nativeCloseLocks(legacy)
}

func TestNativeStartupLocksCoordinateWithLegacyGlobalLock(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "operation.lock")
	legacy, err := nativeLockPaths(context.Background(), []string{legacyPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	locks, err := nativeStartupLocks(ctx, dir, []string{"spark02"})
	cancel()
	if err == nil {
		nativeCloseLocks(locks)
		t.Error("new startup ignored an older supervisor's global lock")
	}
	nativeCloseLocks(legacy)
	current, err := nativeStartupLocks(context.Background(), dir, []string{"spark01"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	legacy, err = nativeLockPaths(ctx, []string{legacyPath})
	cancel()
	if err == nil {
		nativeCloseLocks(legacy)
		t.Error("older supervisor ignored a new startup in progress")
	}
	nativeCloseLocks(current)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	legacy, err = nativeLockPaths(ctx, []string{legacyPath})
	if err != nil {
		t.Fatalf("startup locks were not released: %v", err)
	}
	nativeCloseLocks(legacy)
}
