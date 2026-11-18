package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer that protects the GitHub quota: the unauthenticated API only allows 60 requests/hour/IP,
// and the top bar's "new version available" notice queries once on every full page load. Once the cache expires, a user opening a few
// tabs will exhaust the quota, and then when they actually want to update, the check won't work.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should only hit the source once, actually %d times", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// When the user clicks 'Check for updates' they must get a real-time result, otherwise a just-released version won't be visible until the cache expires.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass the cache, expected 2 source hits, actually %d times", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// Move the stored time back to just expired, simulating the TTL hitting its deadline.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("after TTL expiry it should re-hit the source, expected 2 times, actually %d times", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github unreachable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	// Failure results should also be cached for a while, otherwise when GitHub is unreachable every page load wastes a timeout.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 1 {
		t.Errorf("errors should be cached briefly, expected 1 source hit, actually %d times", calls)
	}

	// But the error TTL must be significantly shorter than the success one, so it can self-heal quickly once the network recovers.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("error TTL(%v) must be shorter than success TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("expected an error to be returned")
	}
	if calls != 2 {
		t.Errorf("after error TTL expiry it should retry, expected 2 times, actually %d times", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// If the visitor closes the tab, the request is canceled. That doesn't mean GitHub has a problem, and 'canceled' must never be
	// written into the cache—otherwise every visitor for the next 30 minutes will receive a baffling error.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // expire the cache, forcing it to hit the source

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("when the caller has canceled, the error should be passed through to it")
	}

	// Key invariant: the canceled attempt leaves no trace—the cache contains neither the 'canceled' error,
	// nor does it lose the previous good result.
	if c.err != nil {
		t.Fatalf("cancel error should not be written to the cache, got %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("cache should retain the previous good result, got %+v", c.rel)
	}

	// That cancellation did not bring back any new data, so the next visitor should reasonably go back to the source—and get a normal result,
	// without being affected by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("A normal request after cancellation should not error: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("Should get a normal result, got %+v", rel)
	}
}
