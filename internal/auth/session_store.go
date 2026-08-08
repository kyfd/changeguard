package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type sessionRepository interface {
	Put(context.Context, string, session, time.Duration) error
	Get(context.Context, string) (session, error)
	Delete(context.Context, string) error
	PutLoginFlow(context.Context, string, loginFlow, time.Duration) error
	TakeLoginFlow(context.Context, string) (loginFlow, error)
	Cleanup(time.Time)
	AllowAttempt(context.Context, string, int, time.Duration) (bool, time.Duration, error)
	ResetAttempts(context.Context, string) error
	Health(context.Context) error
	Close() error
	Mode() string
}

type loginAttempt struct {
	Count     int
	ExpiresAt time.Time
}

type memorySessionRepository struct {
	mu       sync.Mutex
	sessions map[string]session
	flows    map[string]loginFlow
	attempts map[string]loginAttempt
}

func newMemorySessionRepository() *memorySessionRepository {
	return &memorySessionRepository{sessions: make(map[string]session), flows: make(map[string]loginFlow), attempts: make(map[string]loginAttempt)}
}
func (r *memorySessionRepository) Put(_ context.Context, key string, value session, _ time.Duration) error {
	r.mu.Lock()
	r.sessions[key] = value
	r.mu.Unlock()
	return nil
}
func (r *memorySessionRepository) Get(_ context.Context, key string) (session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.sessions[key]
	if !ok || time.Now().After(value.ExpiresAt) {
		delete(r.sessions, key)
		return session{}, errors.New("session not found")
	}
	return value, nil
}
func (r *memorySessionRepository) PutLoginFlow(_ context.Context, key string, value loginFlow, _ time.Duration) error {
	r.mu.Lock()
	r.flows[key] = value
	r.mu.Unlock()
	return nil
}
func (r *memorySessionRepository) TakeLoginFlow(_ context.Context, key string) (loginFlow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.flows[key]
	delete(r.flows, key)
	if !ok || time.Now().After(value.ExpiresAt) {
		return loginFlow{}, errors.New("login flow not found")
	}
	return value, nil
}
func (r *memorySessionRepository) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	delete(r.sessions, key)
	r.mu.Unlock()
	return nil
}
func (r *memorySessionRepository) Cleanup(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range r.sessions {
		if now.After(value.ExpiresAt) {
			delete(r.sessions, key)
		}
	}
	for key, value := range r.flows {
		if now.After(value.ExpiresAt) {
			delete(r.flows, key)
		}
	}
	for key, value := range r.attempts {
		if now.After(value.ExpiresAt) {
			delete(r.attempts, key)
		}
	}
}
func (r *memorySessionRepository) AllowAttempt(_ context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	value := r.attempts[key]
	if value.ExpiresAt.IsZero() || now.After(value.ExpiresAt) {
		value = loginAttempt{ExpiresAt: now.Add(window)}
	}
	value.Count++
	r.attempts[key] = value
	remaining := time.Until(value.ExpiresAt)
	return value.Count <= limit, remaining, nil
}
func (r *memorySessionRepository) ResetAttempts(_ context.Context, key string) error {
	r.mu.Lock()
	delete(r.attempts, key)
	r.mu.Unlock()
	return nil
}
func (r *memorySessionRepository) Health(context.Context) error { return nil }
func (r *memorySessionRepository) Close() error                 { return nil }
func (r *memorySessionRepository) Mode() string                 { return "memory" }

type redisSessionRepository struct {
	client *redis.Client
	prefix string
}

var redisTakeFlowScript = redis.NewScript("local value=redis.call('GET',KEYS[1]); if value then redis.call('DEL',KEYS[1]); end; return value")

var redisAttemptScript = redis.NewScript("local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]); end; return {n,redis.call('PTTL',KEYS[1])}")

func newSessionRepository(logger *log.Logger) sessionRepository {
	if strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_SESSION_MODE"))) != "redis" {
		return newMemorySessionRepository()
	}
	address := strings.TrimSpace(os.Getenv("DBGUARD_REDIS_URL"))
	if address == "" {
		return &failedSessionRepository{err: errors.New("DBGUARD_REDIS_URL is required in redis session mode")}
	}
	options, err := redis.ParseURL(address)
	if err != nil {
		return &failedSessionRepository{err: fmt.Errorf("parse redis url: %w", err)}
	}
	client := redis.NewClient(options)
	repository := &redisSessionRepository{client: client, prefix: "dbguard:"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := repository.Health(ctx); err != nil {
		_ = client.Close()
		logger.Printf("Redis 会话存储初始化失败: %v", err)
		return &failedSessionRepository{err: err}
	}
	return repository
}
func (r *redisSessionRepository) Put(ctx context.Context, key string, value session, ttl time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.prefix+"session:"+key, payload, ttl).Err()
}
func (r *redisSessionRepository) Get(ctx context.Context, key string) (session, error) {
	payload, err := r.client.Get(ctx, r.prefix+"session:"+key).Bytes()
	if err != nil {
		return session{}, err
	}
	var value session
	if err := json.Unmarshal(payload, &value); err != nil {
		return session{}, err
	}
	return value, nil
}
func (r *redisSessionRepository) PutLoginFlow(ctx context.Context, key string, value loginFlow, ttl time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.prefix+"oidc:"+key, payload, ttl).Err()
}
func (r *redisSessionRepository) TakeLoginFlow(ctx context.Context, key string) (loginFlow, error) {
	raw, err := redisTakeFlowScript.Run(ctx, r.client, []string{r.prefix + "oidc:" + key}).Result()
	if err != nil {
		return loginFlow{}, err
	}
	var payload []byte
	switch value := raw.(type) {
	case string:
		payload = []byte(value)
	case []byte:
		payload = value
	default:
		return loginFlow{}, fmt.Errorf("unexpected OIDC flow payload type %T", raw)
	}
	var value loginFlow
	if err := json.Unmarshal(payload, &value); err != nil {
		return loginFlow{}, err
	}
	if time.Now().After(value.ExpiresAt) {
		return loginFlow{}, errors.New("login flow expired")
	}
	return value, nil
}
func (r *redisSessionRepository) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.prefix+"session:"+key).Err()
}
func (r *redisSessionRepository) Cleanup(time.Time) {}
func (r *redisSessionRepository) AllowAttempt(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	result, err := redisAttemptScript.Run(ctx, r.client, []string{r.prefix + "login:" + key}, window.Milliseconds()).Slice()
	if err != nil {
		return false, 0, err
	}
	count, _ := result[0].(int64)
	ttlMS, _ := result[1].(int64)
	return count <= int64(limit), time.Duration(ttlMS) * time.Millisecond, nil
}
func (r *redisSessionRepository) ResetAttempts(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.prefix+"login:"+key).Err()
}
func (r *redisSessionRepository) Health(ctx context.Context) error { return r.client.Ping(ctx).Err() }
func (r *redisSessionRepository) Close() error                     { return r.client.Close() }
func (r *redisSessionRepository) Mode() string                     { return "redis" }

type failedSessionRepository struct{ err error }

func (r *failedSessionRepository) Put(context.Context, string, session, time.Duration) error {
	return r.err
}
func (r *failedSessionRepository) Get(context.Context, string) (session, error) {
	return session{}, r.err
}
func (r *failedSessionRepository) Delete(context.Context, string) error { return r.err }
func (r *failedSessionRepository) PutLoginFlow(context.Context, string, loginFlow, time.Duration) error {
	return r.err
}
func (r *failedSessionRepository) TakeLoginFlow(context.Context, string) (loginFlow, error) {
	return loginFlow{}, r.err
}
func (r *failedSessionRepository) Cleanup(time.Time) {}
func (r *failedSessionRepository) AllowAttempt(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return false, 0, r.err
}
func (r *failedSessionRepository) ResetAttempts(context.Context, string) error { return r.err }
func (r *failedSessionRepository) Health(context.Context) error                { return r.err }
func (r *failedSessionRepository) Close() error                                { return nil }
func (r *failedSessionRepository) Mode() string                                { return "redis-unavailable" }
func (m *Manager) Health(ctx context.Context) error                            { return m.sessionStore.Health(ctx) }
func (m *Manager) Close() error                                                { return m.sessionStore.Close() }
func (m *Manager) SessionMode() string                                         { return m.sessionStore.Mode() }

func clientAddress(r *http.Request) string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DBGUARD_TRUST_PROXY_HEADERS")), "true") {
		if value := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(value) != nil {
			return value
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
