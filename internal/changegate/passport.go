package changegate

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrSignerNotConfigured = errors.New("passport signer is not configured")
	ErrMalformedToken      = errors.New("passport token is malformed")
	ErrInvalidSignature    = errors.New("passport signature is invalid")
)

type PassportClaims struct {
	Version         int    `json:"v"`
	PassportID      string `json:"jti"`
	OrganizationID  string `json:"org"`
	ChangeID        string `json:"change"`
	ArtifactSHA256  string `json:"sha256"`
	Environment     string `json:"env"`
	RuleSetVersion  string `json:"rules"`
	ApproverID      string `json:"approver"`
	IssuedAtUnix    int64  `json:"iat"`
	ExpiresAtUnix   int64  `json:"exp"`
}

type Signer struct {
	key []byte
}

func NewSigner(secret string) (*Signer, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return nil, ErrSignerNotConfigured
	}
	return &Signer{key: []byte(secret)}, nil
}

func NewPassportID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate passport id: %w", err)
	}
	return "pass_" + hex.EncodeToString(buffer), nil
}

func (s *Signer) Sign(claims PassportClaims) (string, error) {
	if s == nil || len(s.key) < 32 {
		return "", ErrSignerNotConfigured
	}
	claims.Version = 1
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := "cg1." + encoded
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + signature, nil
}

func (s *Signer) Verify(token string) (PassportClaims, error) {
	if s == nil || len(s.key) < 32 {
		return PassportClaims{}, ErrSignerNotConfigured
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "cg1" || parts[1] == "" || parts[2] == "" {
		return PassportClaims{}, ErrMalformedToken
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(provided) != sha256.Size {
		return PassportClaims{}, ErrMalformedToken
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return PassportClaims{}, ErrInvalidSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return PassportClaims{}, ErrMalformedToken
	}
	var claims PassportClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil || claims.Version != 1 || claims.PassportID == "" || claims.ChangeID == "" || claims.ArtifactSHA256 == "" || claims.ExpiresAtUnix <= claims.IssuedAtUnix {
		return PassportClaims{}, ErrMalformedToken
	}
	return claims, nil
}

func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func ClaimsTimes(claims PassportClaims) (time.Time, time.Time) {
	return time.Unix(claims.IssuedAtUnix, 0).UTC(), time.Unix(claims.ExpiresAtUnix, 0).UTC()
}
