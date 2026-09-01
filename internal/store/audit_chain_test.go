package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/audit"
	"github.com/kyfd/changeguard/internal/model"
)

type failingAuditBackend struct {
	payload []byte
	version int64
	fail    bool
}

func (b *failingAuditBackend) Load(_ context.Context) ([]byte, int64, error) {
	return b.payload, b.version, nil
}
func (b *failingAuditBackend) Save(_ context.Context, payload []byte, expected int64) (int64, error) {
	if b.fail {
		return expected, errors.New("save failed")
	}
	b.payload = append([]byte(nil), payload...)
	b.version = expected + 1
	return b.version, nil
}
func (b *failingAuditBackend) Health(context.Context) error { return nil }
func (b *failingAuditBackend) Close()                       {}
func (b *failingAuditBackend) Mode() string                 { return "test" }

func TestAuditConcurrentAppendOrderAndRollback(t *testing.T) {
	data := NewMemory()
	org := data.Organizations()[0].ID
	const count = 20
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if err := data.RecordAudit(model.AuditEvent{OrganizationID: org, ID: fmt.Sprintf("concurrent-%02d", index), ActorID: "test", Action: "TEST", CreatedAt: time.Now()}); err != nil {
				t.Errorf("append: %v", err)
			}
		}(index)
	}
	wg.Wait()
	if err := data.VerifyAuditChain(org); err != nil {
		t.Fatal(err)
	}
	ordered := data.AuditsByOrganizationAppendOrder(org)
	if len(ordered) < count {
		t.Fatalf("append-order query returned %d audits", len(ordered))
	}
	if err := audit.Verify(ordered); err != nil {
		t.Fatalf("append-order query must preserve chain: %v", err)
	}

	backend := &failingAuditBackend{}
	persisted := NewMemory()
	persisted.backend = backend
	if err := persisted.saveLocked(); err != nil {
		t.Fatal(err)
	}
	before := len(persisted.Audits(0))
	backend.fail = true
	if err := persisted.RecordAudit(model.AuditEvent{OrganizationID: org, ID: "must-rollback", ActorID: "test", Action: "FAIL", CreatedAt: time.Now()}); err == nil {
		t.Fatal("expected save failure")
	}
	if after := len(persisted.Audits(0)); after != before {
		t.Fatalf("audit append not rolled back: before=%d after=%d", before, after)
	}
}
