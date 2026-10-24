package routing

import (
	"runtime"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/scanloop"
	"github.com/puzpuzpuz/xsync/v4"
)

// LeaseCleaner periodically sweeps for expired leases.
type LeaseCleaner struct {
	router      *Router
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
	minInterval time.Duration
	jitterRange time.Duration

	// test hook: called at the beginning of each sweep.
	sweepHook func()
}

func NewLeaseCleaner(router *Router) *LeaseCleaner {
	return newLeaseCleanerWithIntervals(router, 13*time.Second, 4*time.Second)
}

func newLeaseCleanerWithIntervals(router *Router, minInterval, jitterRange time.Duration) *LeaseCleaner {
	return &LeaseCleaner{
		router:      router,
		stopCh:      make(chan struct{}),
		minInterval: minInterval,
		jitterRange: jitterRange,
	}
}

func (c *LeaseCleaner) Start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		scanloop.Run(c.stopCh, c.minInterval, c.jitterRange, c.sweep)
	}()
}

func (c *LeaseCleaner) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.wg.Wait()
}

func (c *LeaseCleaner) sweep() {
	if c.sweepHook != nil {
		c.sweepHook()
	}

	now := time.Now()
	nowNs := now.UnixNano()

	type platformState struct {
		platID string
		state  *PlatformRoutingState
	}
	states := make([]platformState, 0, c.router.states.Size())
	c.router.states.Range(func(platID string, state *PlatformRoutingState) bool {
		select {
		case <-c.stopCh:
			return false
		default:
		}
		states = append(states, platformState{platID: platID, state: state})
		return true
	})
	if len(states) == 0 {
		return
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > len(states) {
		workers = len(states)
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, item := range states {
		select {
		case <-c.stopCh:
			wg.Wait()
			return
		default:
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(platID string, state *PlatformRoutingState) {
			defer wg.Done()
			defer func() { <-sem }()
			c.sweepPlatformState(platID, state, nowNs)
		}(item.platID, item.state)
	}
	wg.Wait()
}

const leaseExpirySweepBatchLimit = 256

func (c *LeaseCleaner) sweepPlatformState(platID string, state *PlatformRoutingState, nowNs int64) {
	// Expiry deadlines are kept in a min-heap, so a sweep visits only leases
	// that have actually expired instead of scanning the full account map. A
	// bounded batch prevents a mass expiry from monopolizing a CPU core.
	for handled := 0; handled < leaseExpirySweepBatchLimit; handled++ {
		select {
		case <-c.stopCh:
			return
		default:
		}
		account, ok := state.Leases.takeExpiredAccount(nowNs)
		if !ok {
			return
		}
		state.Leases.Compute(account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
			if !loaded || current.ExpiryNs >= nowNs {
				return current, xsync.CancelOp
			}
			state.Leases.stats.Dec(current.EgressIP)
			if c.router.onLeaseEvent != nil {
				c.router.onLeaseEvent(LeaseEvent{
					Type:        LeaseExpire,
					PlatformID:  platID,
					Account:     account,
					NodeHash:    current.NodeHash,
					EgressIP:    current.EgressIP,
					CreatedAtNs: current.CreatedAtNs,
				})
			}
			return current, xsync.DeleteOp
		})
	}
}
