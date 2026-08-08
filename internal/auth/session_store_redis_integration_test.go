package auth

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisSessionRepositoryIntegration(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("DBGUARD_REDIS_TEST_URL"))
	if address == "" {
		t.Skip("DBGUARD_REDIS_TEST_URL is not configured")
	}
	prefix := fmt.Sprintf("changeguard:test:%d:", time.Now().UnixNano())
	t.Setenv("DBGUARD_SESSION_MODE", "redis")
	t.Setenv("DBGUARD_REDIS_URL", address)
	t.Setenv("DBGUARD_REDIS_PREFIX", prefix)

	repository := newSessionRepository(log.New(io.Discard, "", 0))
	if repository.Mode() != "redis" {
		t.Fatalf("session repository mode = %q", repository.Mode())
	}
	redisRepository, ok := repository.(*redisSessionRepository)
	if !ok {
		t.Fatalf("session repository type = %T", repository)
	}
	defer repository.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer deleteRedisPrefix(t, redisRepository.client, prefix)

	expiresAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := repository.Put(ctx, "shared", session{UserID: "user-a", CSRFToken: "csrf-a", ExpiresAt: expiresAt}, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if stored.UserID != "user-a" || stored.CSRFToken != "csrf-a" {
		t.Fatalf("unexpected stored session: %+v", stored)
	}
	ttl, err := redisRepository.client.TTL(ctx, prefix+"session:shared").Result()
	if err != nil || ttl <= 0 || ttl > 10*time.Minute {
		t.Fatalf("session TTL = %s, err=%v", ttl, err)
	}

	flow := loginFlow{Nonce: "nonce", CodeVerifier: "verifier", Next: "/#/dashboard", ExpiresAt: time.Now().Add(time.Minute)}
	if err := repository.PutLoginFlow(ctx, "flow", flow, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TakeLoginFlow(ctx, "flow"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TakeLoginFlow(ctx, "flow"); err == nil {
		t.Fatal("OIDC login flow was not consumed atomically")
	}

	for attempt := 1; attempt <= 3; attempt++ {
		allowed, _, err := repository.AllowAttempt(ctx, "member@example.com", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != (attempt <= 2) {
			t.Fatalf("attempt %d allowed=%t", attempt, allowed)
		}
	}
	if err := repository.ResetAttempts(ctx, "member@example.com"); err != nil {
		t.Fatal(err)
	}
	allowed, _, err := repository.AllowAttempt(ctx, "member@example.com", 2, time.Minute)
	if err != nil || !allowed {
		t.Fatalf("rate limit reset allowed=%t err=%v", allowed, err)
	}

	options, err := redis.ParseURL(address)
	if err != nil {
		t.Fatal(err)
	}
	otherClient := redis.NewClient(options)
	defer otherClient.Close()
	otherPrefix := prefix + "isolated:"
	otherRepository := &redisSessionRepository{client: otherClient, prefix: otherPrefix}
	if err := otherRepository.Put(ctx, "shared", session{UserID: "user-b", CSRFToken: "csrf-b", ExpiresAt: expiresAt}, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	defer deleteRedisPrefix(t, otherClient, otherPrefix)
	isolated, err := otherRepository.Get(ctx, "shared")
	if err != nil || isolated.UserID != "user-b" {
		t.Fatalf("isolated session = %+v, err=%v", isolated, err)
	}
	stillOriginal, err := repository.Get(ctx, "shared")
	if err != nil || stillOriginal.UserID != "user-a" {
		t.Fatalf("primary namespace was overwritten: %+v, err=%v", stillOriginal, err)
	}
}

func deleteRedisPrefix(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	iterator := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
	keys := make([]string, 0)
	for iterator.Next(ctx) {
		keys = append(keys, iterator.Val())
	}
	if err := iterator.Err(); err != nil {
		t.Errorf("scan Redis cleanup prefix: %v", err)
		return
	}
	if len(keys) > 0 {
		if err := client.Del(ctx, keys...).Err(); err != nil {
			t.Errorf("delete Redis cleanup keys: %v", err)
		}
	}
}
