package agent

import (
	"context"
	"github.com/redis/go-redis/v9"
	"testing"
)

func TestUsageSnapshotContextBackendFailure(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer client.Close()
	runtime := &Runtime{limitClient: client}
	if _, err := runtime.UsageSnapshotContext(context.Background(), "org", "user"); err == nil {
		t.Fatal("backend failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.UsageSnapshotContext(ctx, "org", "user"); err == nil {
		t.Fatal("cancellation hidden")
	}
	runtime = &Runtime{limitRequired: true}
	if _, err := runtime.UsageSnapshotContext(context.Background(), "org", "user"); err == nil {
		t.Fatal("required backend missing")
	}
}
