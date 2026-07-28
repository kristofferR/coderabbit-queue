package serve

import (
	"context"
	"errors"
	"testing"
	"time"
)

type discovererFunc func(context.Context) (Listing, error)

func (f discovererFunc) Discover(ctx context.Context) (Listing, error) {
	return f(ctx)
}

func TestDiscoverFlightOutlivesItsFirstCaller(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	discoverer := discovererFunc(func(ctx context.Context) (Listing, error) {
		close(started)
		select {
		case <-ctx.Done():
			return Listing{}, ctx.Err()
		case <-release:
			return Listing{Repos: []Candidate{{Repo: "o/r"}}}, nil
		}
	})
	var cache discoverCache
	now := time.Now().UTC()

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := cache.get(leaderCtx, discoverer, now, false)
		leader <- err
	}()
	<-started

	waiter := make(chan error, 1)
	go func() {
		listing, err := cache.get(t.Context(), discoverer, now, false)
		if err == nil && (len(listing.Repos) != 1 || listing.Repos[0].Repo != "o/r") {
			err = errors.New("shared discovery returned the wrong listing")
		}
		waiter <- err
	}()

	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error = %v, want cancellation", err)
	}
	close(release)
	if err := <-waiter; err != nil {
		t.Fatalf("remaining waiter lost the shared discovery: %v", err)
	}
}
