package pool

import (
	"context"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Shared rated-bids read: concurrent opens for the same model wait on one
// router read instead of each starting their own. The read runs detached
// from any one caller (bounded by readCallTimeout); each caller waits on
// its own context. Every caller gets its own copy of the rows.

type ratedCall struct {
	done chan struct{}
	rows []router.RatedBid
	err  error
}

func (p *Pool) sharedRatedBids(ctx context.Context, modelID string) ([]router.RatedBid, error) {
	p.ratedMu.Lock()
	if p.ratedCalls == nil {
		p.ratedCalls = map[string]*ratedCall{}
	}
	c, ok := p.ratedCalls[modelID]
	if !ok {
		c = &ratedCall{done: make(chan struct{})}
		p.ratedCalls[modelID] = c
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readCallTimeout)
			c.rows, c.err = p.client.RatedBidsCtx(rctx, modelID)
			cancel()
			p.ratedMu.Lock()
			delete(p.ratedCalls, modelID)
			p.ratedMu.Unlock()
			close(c.done)
		}()
	}
	p.ratedMu.Unlock()
	select {
	case <-c.done:
		// Rows and error pass through together: an incomplete read keeps
		// its valid rows (router.ErrRatedBidsIncomplete).
		return append([]router.RatedBid(nil), c.rows...), c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
