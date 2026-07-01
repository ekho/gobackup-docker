package docker

import (
	"context"
	"log"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// WatchEvents subscribes to the Docker event stream and calls fire() whenever a
// container starts or dies, and once on every (re)connect so a reconnect after a
// dropped stream re-syncs any changes missed while disconnected.
//
// The SDK does not auto-reconnect: when the stream errors we log, back off, and
// re-subscribe. Blocks until ctx is cancelled.
func (c *Client) WatchEvents(ctx context.Context, fire func()) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second

	for ctx.Err() == nil {
		f := filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("event", string(events.ActionStart)),
			filters.Arg("event", string(events.ActionDie)),
		)
		msgs, errs := c.cli.Events(ctx, events.ListOptions{Filters: f})
		backoff = time.Second // reset after a successful subscribe
		fire()                // reconcile on (re)connect

		if c.pump(ctx, msgs, errs, fire) {
			return // ctx cancelled
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// pump drains one event subscription. Returns true if ctx was cancelled (caller
// should stop), false if the stream errored (caller should reconnect).
func (c *Client) pump(ctx context.Context, msgs <-chan events.Message, errs <-chan error, fire func()) bool {
	for {
		select {
		case <-ctx.Done():
			return true
		case <-msgs:
			fire()
		case err := <-errs:
			if ctx.Err() != nil {
				return true
			}
			log.Printf("[docker] event stream error: %v; reconnecting", err)
			return false
		}
	}
}
