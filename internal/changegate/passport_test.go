package changegate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignerRejectsWeakSecretAndTampering(t *testing.T) {
	if _, err := NewSigner("too-short"); !errors.Is(err, ErrSignerNotConfigured) {
		t.Fatalf("weak key must be rejected, got %v", err)
	}
	signer, err := NewSigner(strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	claims := PassportClaims{PassportID: "pass_test", OrganizationID: "org_test", ChangeID: "chg_test", ArtifactSHA256: strings.Repeat("a", 64), Environment: "生产环境", RuleSetVersion: "sha256:rules", ApproverID: "reviewer", IssuedAtUnix: now.Unix(), ExpiresAtUnix: now.Add(10 * time.Minute).Unix()}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := signer.Verify(token)
	if err != nil || verified.PassportID != claims.PassportID || verified.Version != 1 {
		t.Fatalf("signed claims did not round trip: claims=%+v err=%v", verified, err)
	}
	parts := strings.Split(token, ".")
	if parts[1][0] == 'A' {
		parts[1] = "B" + parts[1][1:]
	} else {
		parts[1] = "A" + parts[1][1:]
	}
	if _, err := signer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered payload must fail signature validation, got %v", err)
	}
}

func TestTokenSHA256DoesNotExposeToken(t *testing.T) {
	token := "cg1.payload.signature"
	hash := TokenSHA256(token)
	if hash == token || len(hash) != 64 {
		t.Fatalf("unexpected token digest %q", hash)
	}
}
