package provider

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeCreds hands out credentials that expire `ttl` from now and counts how often it is asked.
type fakeCreds struct {
	ttl   time.Duration
	calls atomic.Int32
}

func (f *fakeCreds) Retrieve(context.Context) (aws.Credentials, error) {
	f.calls.Add(1)
	return aws.Credentials{
		AccessKeyID: "AKIA", SecretAccessKey: "s", SessionToken: "t",
		CanExpire: true, Expires: time.Now().Add(f.ttl),
	}, nil
}

// N5: credentials that expire inside the ExpiryWindow must be treated as already expired so
// the cache refreshes ahead of time; without the window they would be reused right up to the
// expiry instant and could reach Bedrock already expired.
func TestCredentialsCacheRefreshesInsideExpiryWindow(t *testing.T) {
	ctx := context.Background()

	// Credentials valid for 2 minutes — inside our 5-minute window.
	withWindow := &fakeCreds{ttl: 2 * time.Minute}
	cache := aws.NewCredentialsCache(withWindow, credentialsCacheOptions)
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatal(err)
	}
	if got := withWindow.calls.Load(); got != 2 {
		t.Fatalf("2-minute credentials are inside the 5-minute window and must be refreshed on every Retrieve, got %d provider calls", got)
	}

	// Control: the same credentials without the window are cached (the SDK default we are fixing).
	noWindow := &fakeCreds{ttl: 2 * time.Minute}
	plain := aws.NewCredentialsCache(noWindow)
	_, _ = plain.Retrieve(ctx)
	_, _ = plain.Retrieve(ctx)
	if got := noWindow.calls.Load(); got != 1 {
		t.Fatalf("control: default cache should reuse 2-minute credentials, got %d provider calls", got)
	}

	// Credentials well outside the window are cached normally (no needless STS churn).
	longLived := &fakeCreds{ttl: time.Hour}
	cache2 := aws.NewCredentialsCache(longLived, credentialsCacheOptions)
	_, _ = cache2.Retrieve(ctx)
	_, _ = cache2.Retrieve(ctx)
	if got := longLived.calls.Load(); got != 1 {
		t.Fatalf("1-hour credentials must be cached, got %d provider calls", got)
	}
}
