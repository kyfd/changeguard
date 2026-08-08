package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestVerifyGitLabSigningToken(t *testing.T) {
	key := []byte("gitlab-signing-key")
	token := "whsec_" + base64.StdEncoding.EncodeToString(key)
	body := []byte(`{"object_kind":"pipeline"}`)
	now := time.Now().Truncate(time.Second)
	messageID := "evt-1"
	timestamp := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(messageID + "." + timestamp + "." + string(body)))
	headers := http.Header{
		"Webhook-Id":        []string{messageID},
		"Webhook-Timestamp": []string{timestamp},
		"Webhook-Signature": []string{"v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}
	config := Config{GitLabSigningToken: token, MaxWebhookAge: 5 * time.Minute}
	if err := VerifyGitLab(config, headers, body, now); err != nil {
		t.Fatalf("expected signature to pass: %v", err)
	}
	if err := VerifyGitLab(config, headers, body, now.Add(10*time.Minute)); err != ErrReplay {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestVerifyGitLabLegacyToken(t *testing.T) {
	headers := http.Header{"X-Gitlab-Token": []string{"legacy-secret"}}
	if err := VerifyGitLab(Config{GitLabSecretToken: "legacy-secret"}, headers, nil, time.Now()); err != nil {
		t.Fatalf("expected legacy token to pass: %v", err)
	}
}

func TestParseGitLabPipeline(t *testing.T) {
	body := []byte(`{
		"object_kind":"pipeline",
		"object_attributes":{
			"id":31,
			"sha":"abcdef",
			"status":"success",
			"finished_at":"2026-08-08T10:00:00Z",
			"url":"https://gitlab.example.com/acme/orders/-/pipelines/31",
			"variables":[{"key":"CHANGEGUARD_CHANGE_ID","value":"chg_123"}]
		},
		"project":{"path_with_namespace":"acme/orders"}
	}`)
	headers := http.Header{
		"X-Gitlab-Event": []string{"Pipeline Hook"},
		"Webhook-Id":     []string{"evt-31"},
	}
	event, err := ParseGitLab(Config{GitLabOrganization: "org_demo"}, headers, body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if event.ChangeID != "chg_123" || event.Pipeline != "31" || event.Status != "SUCCESS" || event.OccurredAt.Format(time.RFC3339) != "2026-08-08T10:00:00Z" {
		t.Fatalf("unexpected event: %+v", event)
	}
}

func TestVerifyAndParseJenkins(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer jenkins-secret"}}
	config := Config{JenkinsToken: "jenkins-secret", JenkinsOrganization: "org_demo"}
	if err := VerifyJenkins(config, headers); err != nil {
		t.Fatalf("expected token to pass: %v", err)
	}
	body := []byte(`{
		"change_id":"chg_123",
		"job_name":"orders-production",
		"build_number":128,
		"build_url":"https://jenkins.example.com/job/orders/128/",
		"status":"SUCCESS",
		"commit_sha":"abcdef",
		"environment":"production",
		"occurred_at":"2026-08-08T10:05:00Z"
	}`)
	event, err := ParseJenkins(config, headers, body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if event.Project != "orders-production" || event.Pipeline != "128" || event.OccurredAt.Format(time.RFC3339) != "2026-08-08T10:05:00Z" {
		t.Fatalf("unexpected event: %+v", event)
	}
}
