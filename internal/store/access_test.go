package store

import (
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestUpdateMemberPersistsApplicationGrants(t *testing.T) {
	data := NewMemory()
	users := data.Users()
	applications := data.Applications()
	var reviewer model.User
	for _, user := range users {
		if user.Role == model.RoleReviewer {
			reviewer = user
			break
		}
	}
	if reviewer.ID == "" || len(applications) == 0 {
		t.Fatal("seed data missing")
	}
	_, err := data.UpdateMember(reviewer.OrganizationID, reviewer.ID, func(user *model.User) error { return nil }, []model.ApplicationGrantInput{{ApplicationID: applications[0].ID, CanReview: true}}, model.AuditEvent{OrganizationID: reviewer.OrganizationID, ID: NewID("audit_"), ActorID: "usr_owner", ActorName: "owner", Action: "UPDATE_MEMBER", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	grants := data.ApplicationGrantsByUser(reviewer.OrganizationID, reviewer.ID)
	if len(grants) != 1 || grants[0].ApplicationID != applications[0].ID || !grants[0].CanReview || grants[0].CanSubmit {
		t.Fatalf("unexpected grants: %#v", grants)
	}
	organization, _ := data.Organization(reviewer.OrganizationID)
	if !organization.ApplicationAccessControlled {
		t.Fatal("application access control was not activated")
	}
}
