package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileStateBackendAllowsOnlyOneInstance(t *testing.T) {
	t.Setenv("CHANGEGUARD_STORE_MODE", "file")
	path := filepath.Join(t.TempDir(), "state.json")

	first, err := NewFromEnvironment(context.Background(), path)
	if err != nil {
		t.Fatalf("first instance: %v", err)
	}
	if _, err := os.Stat(path + ".instance.lock"); err != nil {
		t.Fatalf("instance lock file was not created: %v", err)
	}

	second, err := NewFromEnvironment(context.Background(), path)
	if err == nil {
		second.Close()
		t.Fatal("a second file-backed instance must fail closed")
	}
	if !errors.Is(err, ErrInstanceLocked) {
		t.Fatalf("expected ErrInstanceLocked, got %v", err)
	}
	message := err.Error()
	if !strings.Contains(message, path+".instance.lock") {
		t.Fatalf("rejection must name the lock path: %q", message)
	}
	if !strings.Contains(message, "postgres") {
		t.Fatalf("rejection must point at the multi-instance backend: %q", message)
	}

	first.Close()
	third, err := NewFromEnvironment(context.Background(), path)
	if err != nil {
		t.Fatalf("lock must be released on Close: %v", err)
	}
	third.Close()
	// A released lock is released exactly once: a fourth open still works, and a
	// double Close of the same store must not release someone else's lock.
	fourth, err := NewFromEnvironment(context.Background(), path)
	if err != nil {
		t.Fatalf("fourth instance: %v", err)
	}
	fourth.Close()
	fourth.Close()
	fifth, err := NewFromEnvironment(context.Background(), path)
	if err != nil {
		t.Fatalf("double Close must not break the lock lifecycle: %v", err)
	}
	fifth.Close()
}

func TestFileStateBackendLockIsPerStateFile(t *testing.T) {
	t.Setenv("CHANGEGUARD_STORE_MODE", "file")
	dir := t.TempDir()

	first, err := NewFromEnvironment(context.Background(), filepath.Join(dir, "one.json"))
	if err != nil {
		t.Fatalf("first state file: %v", err)
	}
	defer first.Close()
	other, err := NewFromEnvironment(context.Background(), filepath.Join(dir, "two.json"))
	if err != nil {
		t.Fatalf("a different state file must not be blocked: %v", err)
	}
	other.Close()
}

func TestFileStateBackendSkipsLockForInMemoryState(t *testing.T) {
	t.Setenv("CHANGEGUARD_STORE_MODE", "file")
	inMemory, err := NewFromEnvironment(context.Background(), "")
	if err != nil {
		t.Fatalf("in-memory store: %v", err)
	}
	defer inMemory.Close()
	if inMemory.instanceLock != nil {
		t.Fatal("an in-memory store must not hold a file instance lock")
	}
	if inMemory.Mode() != "memory" {
		t.Fatalf("mode = %q", inMemory.Mode())
	}
}

func TestFileStateBackendRejectsUnsupportedStoreMode(t *testing.T) {
	t.Setenv("CHANGEGUARD_STORE_MODE", "sqlite")
	if _, err := NewFromEnvironment(context.Background(), filepath.Join(t.TempDir(), "state.json")); err == nil {
		t.Fatal("an unsupported store mode must fail closed")
	}
}

func TestInstanceLockReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lock, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second release must be a no-op: %v", err)
	}
	var nilLock *instanceLock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil release must be a no-op: %v", err)
	}
}

func TestConcurrentInstanceLockAcquisitionHasExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const attempts = 8

	var (
		wait    sync.WaitGroup
		mutex   sync.Mutex
		winners []*instanceLock
		busy    int
		start   = make(chan struct{})
	)
	for i := 0; i < attempts; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			lock, err := acquireInstanceLock(path)
			mutex.Lock()
			defer mutex.Unlock()
			if err == nil {
				winners = append(winners, lock)
				return
			}
			if errors.Is(err, ErrInstanceLocked) {
				busy++
				return
			}
			t.Errorf("unexpected error: %v", err)
		}()
	}
	close(start)
	wait.Wait()

	if len(winners) != 1 {
		t.Fatalf("exactly one instance must win, got %d", len(winners))
	}
	if busy != attempts-1 {
		t.Fatalf("busy rejections = %d, want %d", busy, attempts-1)
	}
	if err := winners[0].Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}
