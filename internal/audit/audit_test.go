package audit

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestLegacyJSONAndHashChain(t *testing.T) {
	var legacy model.AuditEvent
	if err := json.Unmarshal([]byte(`{"organization_id":"org","id":"old","actor_id":"u","actor_name":"User","action":"create","detail":"legacy","created_at":"2026-01-01T00:00:00Z"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Hash != "" {
		t.Fatal("legacy hash must remain empty")
	}
	next, err := Link(model.AuditEvent{OrganizationID: "org", ID: "new", ActorID: "u", ActorName: "User", Action: "APPROVE", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}, &legacy)
	if err != nil {
		t.Fatal(err)
	}
	if next.Hash == "" || next.PrevHash == "" {
		t.Fatalf("new event was not linked: %+v", next)
	}
	if err := Verify([]model.AuditEvent{legacy, next}); err != nil {
		t.Fatal(err)
	}
	next.Detail = "tampered"
	if err := Verify([]model.AuditEvent{legacy, next}); err == nil {
		t.Fatal("tampered field must fail")
	}
	next.Detail = ""
	linked, err := Link(next, &legacy)
	if err != nil {
		t.Fatal(err)
	}
	linked.Hash = ""
	if err := Verify([]model.AuditEvent{legacy, next, linked}); err == nil {
		t.Fatal("empty hash after hashed chain began must fail")
	}
}

func TestCanonicalExcludesHash(t *testing.T) {
	event := model.AuditEvent{OrganizationID: "org", ID: "a", ActorID: "u", Action: "CREATE", CreatedAt: time.Unix(1, 0), Hash: "one"}
	first, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	event.Hash = "two"
	second, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("Hash must be excluded from canonical payload")
	}
}
