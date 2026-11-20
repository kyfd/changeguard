package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

type failingSaveBackend struct {
	payload []byte
	version int64
	err     error
}

func (b *failingSaveBackend) Load(context.Context) ([]byte, int64, error) {
	return append([]byte(nil), b.payload...), b.version, nil
}
func (b *failingSaveBackend) Save(context.Context, []byte, int64) (int64, error) {
	return 0, b.err
}
func (b *failingSaveBackend) Health(context.Context) error { return nil }
func (b *failingSaveBackend) Close()                       {}
func (b *failingSaveBackend) Mode() string                 { return "failing-test" }

func TestFailedPersistenceDoesNotLeakGhostState(t *testing.T) {
	initial := seedState()
	payload, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("database unavailable")
	data := &Store{
		data: initial, backend: &failingSaveBackend{payload: payload, version: 7, err: backendErr},
		version: 7, persisted: append([]byte(nil), payload...),
	}
	change := data.Changes()[0]
	_, err = data.UpdateChange(change.ID, func(item *model.ChangeRequest) error {
		item.Status = model.StatusCompleted
		return nil
	})
	if !errors.Is(err, backendErr) {
		t.Fatalf("expected persistence error, got %v", err)
	}
	got, err := data.Change(change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != change.Status || got.Version != change.Version {
		t.Fatalf("failed write leaked into reads: before=%s/v%d after=%s/v%d", change.Status, change.Version, got.Status, got.Version)
	}
}

type fencedRefreshBackend struct {
	mu       sync.Mutex
	payload  []byte
	version  int64
	sig      string
	entered  chan struct{}
	release  chan struct{}
	loadOnce sync.Once
}

func (b *fencedRefreshBackend) Load(context.Context) ([]byte, int64, error) {
	b.loadOnce.Do(func() {
		close(b.entered)
		<-b.release
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.payload...), b.version, nil
}

func (b *fencedRefreshBackend) Save(_ context.Context, payload []byte, expected int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payload = append([]byte(nil), payload...)
	b.version = expected + 1
	return b.version, nil
}

func (b *fencedRefreshBackend) Signature(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sig, nil
}

func (b *fencedRefreshBackend) Health(context.Context) error { return nil }
func (b *fencedRefreshBackend) Close()                       {}
func (b *fencedRefreshBackend) Mode() string                 { return "fenced-refresh-test" }

func TestStartRefreshDoesNotClobberConcurrentLocalMutation(t *testing.T) {
	initial := seedState()
	stalePayload, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fencedRefreshBackend{
		payload: stalePayload,
		version: 1,
		sig:     "sig-1",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	data := &Store{
		data:      initial,
		backend:   backend,
		version:   1,
		persisted: append([]byte(nil), stalePayload...),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data.StartRefresh(ctx, 10*time.Millisecond)

	backend.mu.Lock()
	backend.sig = "sig-2" // force next tick to call Load()
	backend.mu.Unlock()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("StartRefresh did not call Load")
	}

	// Mutate locally while Load is in flight with stale payload.
	target := data.Changes()[0]
	updated, err := data.UpdateChange(target.ID, func(c *model.ChangeRequest) error {
		c.Title = "newer-local-title"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	close(backend.release)
	time.Sleep(40 * time.Millisecond)

	got, err := data.Change(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != updated.Title {
		t.Fatalf("StartRefresh clobbered local mutation: got=%q want=%q", got.Title, updated.Title)
	}
}
