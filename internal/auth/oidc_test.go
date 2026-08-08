package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/store"
)

func encodeOIDCTestPart(t *testing.T, value any) string {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(content)
}

func signOIDCTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := encodeOIDCTestPart(t, map[string]any{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	payload := encodeOIDCTestPart(t, claims)
	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCIDTokenSignatureAndClaims(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA",
			"kid": "test-key",
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer jwksServer.Close()

	manager := New(Config{
		Mode:                 "oidc",
		Issuer:               "https://identity.example.com",
		ClientID:             "dbguard-client",
		RedirectURL:          "https://dbguard.example.com/auth/callback",
		SessionTTL:           time.Hour,
		HTTPTimeout:          time.Second,
		RequireVerifiedEmail: true,
	}, store.NewMemory(), log.New(io.Discard, "", 0))
	claims := map[string]any{
		"iss":            manager.config.Issuer,
		"aud":            manager.config.ClientID,
		"sub":            "identity-user-1",
		"nonce":          "nonce-value",
		"email":          "owner@example.com",
		"email_verified": true,
		"exp":            time.Now().Add(5 * time.Minute).Unix(),
		"nbf":            time.Now().Add(-time.Minute).Unix(),
	}
	token := signOIDCTestToken(t, privateKey, claims)
	verified, err := manager.verifyIDToken(context.Background(), discoveryDocument{JWKSURI: jwksServer.URL}, token, "nonce-value")
	if err != nil {
		t.Fatalf("valid ID token rejected: %v", err)
	}
	if verified["sub"] != "identity-user-1" {
		t.Fatalf("unexpected subject: %v", verified["sub"])
	}
	if _, err := manager.verifyIDToken(context.Background(), discoveryDocument{JWKSURI: jwksServer.URL}, token, "wrong-nonce"); err == nil {
		t.Fatal("nonce mismatch must be rejected")
	}

	missingVerification := make(map[string]any, len(claims))
	for key, value := range claims {
		missingVerification[key] = value
	}
	delete(missingVerification, "email_verified")
	unverifiedToken := signOIDCTestToken(t, privateKey, missingVerification)
	if _, err := manager.verifyIDToken(context.Background(), discoveryDocument{JWKSURI: jwksServer.URL}, unverifiedToken, "nonce-value"); err == nil {
		t.Fatal("missing email_verified claim must be rejected")
	}

	parts := []byte(token)
	parts[len(parts)-1] ^= 1
	if _, err := manager.verifyIDToken(context.Background(), discoveryDocument{JWKSURI: jwksServer.URL}, string(parts), "nonce-value"); err == nil {
		t.Fatal("tampered signature must be rejected")
	}
}
