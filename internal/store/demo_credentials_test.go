package store

import (
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestMergeDemoCredentialsRefreshesPersistedPassword(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_ACCOUNTS", "true")
	data := state{
		Users:       []model.User{{ID: "usr_developer"}},
		Credentials: []model.UserCredential{{UserID: "usr_developer", PasswordSalt: "old-salt", PasswordHash: "old-hash"}},
	}

	mergeDemoCredentials(&data)

	if len(data.Credentials) != 1 {
		t.Fatalf("unexpected credentials: %+v", data.Credentials)
	}
	if data.Credentials[0].PasswordSalt != demoCredentials[0].PasswordSalt || data.Credentials[0].PasswordHash != demoCredentials[0].PasswordHash {
		t.Fatalf("persisted demo credential was not refreshed: %+v", data.Credentials[0])
	}
}

func TestDemoAccountsAlsoEnableRequiredDemoData(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "")
	t.Setenv("DBGUARD_ENABLE_DEMO_ACCOUNTS", "true")

	data := initialState()

	if len(data.Organizations) == 0 || len(data.Users) < 3 || len(data.Applications) == 0 {
		t.Fatalf("demo accounts require their organization, users and applications: %+v", data)
	}
}
