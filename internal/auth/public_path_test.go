package auth

import "testing"

func TestOperationsWebhookIsPublicButEventListingIsProtected(t *testing.T) {
	manager := &Manager{}
	if !manager.publicPath("/api/integrations/operations/webhook") {
		t.Fatal("operations webhook must reach its independent bearer authentication")
	}
	if manager.publicPath("/api/integrations/operations/events") {
		t.Fatal("operations event listing must require an enterprise session")
	}
}
