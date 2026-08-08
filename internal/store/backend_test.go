package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
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
