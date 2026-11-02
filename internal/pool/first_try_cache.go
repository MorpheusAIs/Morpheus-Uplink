package pool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Display cache of FirstTry results (spec §4 Load): 60 s per model, errors
// held at most 10 s, at most 4 rated reads at once, one read per model at a
// time on a detached context, and no read for a model with no healthy bid.
// An open never uses it.

const (
	firstTryTTL      = 60 * time.Second
	firstTryErrTTL   = 10 * time.Second
	firstTryMaxReads = 4
)

var errNoHealthyBid = errors.New("no healthy bid for the model")

type firstTryEntry struct {
	res  FirstTry
	err  error
	at   time.Time     // zero: no result yet
	done chan struct{} // non-nil while a read is in flight
}

type firstTryCache struct {
	mu  sync.Mutex
	m   map[string]*firstTryEntry
	sem chan struct{}
}

func (p *Pool) ftCache() *firstTryCache {
	p.ftOnce.Do(func() {
		p.ftc = &firstTryCache{m: map[string]*firstTryEntry{}, sem: make(chan struct{}, firstTryMaxReads)}
	})
	return p.ftc
}

// CachedFirstTry is the held result for the model while it is under 60 s
// old (kept while a refresh is in flight); it never reads.
func (p *Pool) CachedFirstTry(modelID string) (FirstTry, bool) {
	c := p.ftCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[normModel(modelID)]
	if e == nil || e.at.IsZero() || e.err != nil || p.now().Sub(e.at) >= firstTryTTL {
		return FirstTry{}, false
	}
	return e.res, true
}

// FillFirstTry returns the held result, else waits for (or starts) the
// model's one read. A caller's cancel ends only its wait: the read goes on
// and its result is held.
func (p *Pool) FillFirstTry(ctx context.Context, modelID string) (FirstTry, error) {
	c := p.ftCache()
	key := normModel(modelID)
	c.mu.Lock()
	e := c.m[key]
	if e != nil && !e.at.IsZero() {
		age := p.now().Sub(e.at)
		if e.err == nil && age < firstTryTTL {
			c.mu.Unlock()
			return e.res, nil
		}
		if e.err != nil && age < firstTryErrTTL {
			c.mu.Unlock()
			return FirstTry{}, e.err
		}
	}
	if e == nil || e.done == nil {
		c.mu.Unlock()
		if h, err := p.healthyBids(modelID); err != nil || len(h) == 0 {
			return FirstTry{}, errNoHealthyBid
		}
		c.mu.Lock()
		if e = c.m[key]; e == nil {
			e = &firstTryEntry{}
			c.m[key] = e
		}
		if e.done == nil {
			done := make(chan struct{})
			e.done = done
			go p.readFirstTry(context.WithoutCancel(ctx), c, e, modelID, done)
		}
	}
	done := e.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return FirstTry{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.res, e.err
}

func (p *Pool) readFirstTry(ctx context.Context, c *firstTryCache, e *firstTryEntry, modelID string, done chan struct{}) {
	c.sem <- struct{}{}
	res, err := p.FirstTry(ctx, modelID)
	<-c.sem
	c.mu.Lock()
	e.res, e.err, e.at, e.done = res, err, p.now(), nil
	c.mu.Unlock()
	close(done)
}
