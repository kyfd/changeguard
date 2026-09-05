package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

func TestUsePassportDifferentConsumersCompeteForOneConsumption(t *testing.T) {
	data := NewMemory()
	now := time.Now().UTC()
	change, passport := passportFixture(now, changegate.RuleSetVersion(data.PoliciesByOrganization("org_demo")))
	if err := data.CreateChange(change, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "create_competition", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := data.CreatePassport(passport, model.AuditEvent{OrganizationID: change.OrganizationID, ID: "issue_competition", ChangeID: change.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const consumers = 100
	start := make(chan struct{})
	results := make(chan error, consumers)
	var workers sync.WaitGroup
	for i := 0; i < consumers; i++ {
		workers.Add(1)
		go func(consumer string) {
			defer workers.Done()
			<-start
			_, err := data.UsePassport(passport.ID, passport.TokenSHA256, consumer, now.Add(time.Second), true,
				model.AuditEvent{OrganizationID: change.OrganizationID, ID: "audit_" + consumer, ChangeID: change.ID, CreatedAt: now.Add(time.Second)})
			results <- err
		}(fmt.Sprintf("pipeline-%d", i))
	}
	close(start)
	workers.Wait()
	close(results)
	allowed, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			allowed++
		case errors.Is(err, ErrPassportReplay):
			conflicts++
		default:
			t.Fatalf("unexpected consume error: %v", err)
		}
	}
	if allowed != 1 || conflicts != consumers-1 {
		t.Fatalf("allowed=%d conflicts=%d", allowed, conflicts)
	}
	completed, err := data.Change(change.ID)
	if err != nil || completed.Status != model.StatusCompleted || completed.Version != change.Version+1 {
		t.Fatalf("change must complete once: %+v, %v", completed, err)
	}
	consumptionAudits := 0
	for _, event := range data.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumptionAudits++
		}
	}
	if consumptionAudits != 1 {
		t.Fatalf("consumption audits=%d", consumptionAudits)
	}
}
