package probe

import (
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/probepolicy"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
	"golang.org/x/sync/singleflight"
)

const ProbeTransferLimit int64 = 64 << 10

type StatsFetcher func(node.Hash, string) ([]byte, time.Duration, int64, int64, error)

type budgetBlock struct {
	month string
	limit int64
	until time.Time
}
type MeteredController struct {
	backgroundSlots chan struct{}
	slotWait        time.Duration
	lifecycleMu     sync.Mutex
	stopped         bool
	active          sync.WaitGroup
	pool            *topology.GlobalNodePool
	subs            *topology.SubscriptionManager
	repo            *state.StateRepo
	mu              sync.Mutex
	states          map[string]state.ProbeNodeState
	flights         singleflight.Group
	slots           chan struct{}
	blocked         map[string]budgetBlock
}

func NewMeteredController(pool *topology.GlobalNodePool, subs *topology.SubscriptionManager, repo *state.StateRepo) (*MeteredController, error) {
	states, err := repo.LoadProbeNodeStates()
	if err != nil {
		return nil, err
	}
	return &MeteredController{pool: pool, subs: subs, repo: repo, states: states, slots: make(chan struct{}, 8), backgroundSlots: make(chan struct{}, 2), slotWait: 15 * time.Second, blocked: map[string]budgetBlock{}}, nil
}

// Shared nodes are charged once, to the lexicographically first enabled owner.
// Any enabled metered owner opts the node out of global background probing.
func (c *MeteredController) Policy(h node.Hash) (string, probepolicy.ProbePolicy, bool) {
	if c == nil {
		return "", probepolicy.ProbePolicy{}, false
	}
	e, ok := c.pool.GetEntry(h)
	if !ok {
		return "", probepolicy.ProbePolicy{}, false
	}
	ids := e.SubscriptionIDs()
	sort.Strings(ids)
	for _, id := range ids {
		s := c.subs.Lookup(id)
		if s == nil || !s.Enabled() {
			continue
		}
		p := s.ProbePolicy()
		if p.Mode == "metered" {
			return id, p, true
		}
	}
	return "", probepolicy.ProbePolicy{}, false
}
func (c *MeteredController) nodeState(h node.Hash) state.ProbeNodeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[h.Hex()]
}
func (c *MeteredController) update(h node.Hash, fn func(*state.ProbeNodeState)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.states[h.Hex()]
	fn(&s)
	if err := c.repo.SaveProbeNodeState(h.Hex(), s); err != nil {
		return err
	}
	c.states[h.Hex()] = s
	return nil
}
func (c *MeteredController) Touch(h node.Hash) error {
	if _, _, ok := c.Policy(h); !ok {
		return nil
	}
	now := time.Now().UnixNano()
	// Coalesce hot-path writes. Conservatively retain up to one extra minute of activity.
	if now-c.nodeState(h).LastUsedNs < int64(time.Minute) {
		return nil
	}
	return c.update(h, func(s *state.ProbeNodeState) { s.LastUsedNs = now })
}
func (c *MeteredController) Due(h node.Hash) bool {
	id, p, ok := c.Policy(h)
	if !ok {
		return false
	}
	if c.budgetBlocked(id, p) {
		return false
	}
	s := c.nodeState(h)
	now := time.Now()
	if s.LastUsedNs == 0 || now.Sub(time.Unix(0, s.LastUsedNs)) > time.Duration(p.ActiveWindow) {
		return false
	}
	if s.NextRetryNs > 0 {
		return now.UnixNano() >= s.NextRetryNs
	}
	verified := s.VerifiedNs
	if verified == 0 {
		if e, ok := c.pool.GetEntry(h); ok {
			verified = e.LastEgressUpdate.Load()
		}
	}
	return verified == 0 || now.Sub(time.Unix(0, verified)) >= time.Duration(p.EgressInterval)
}
func (c *MeteredController) Fresh(h node.Hash, age time.Duration) bool {
	e, ok := c.pool.GetEntry(h)
	if !ok || !e.IsHealthy() || !e.GetEgressIP().IsValid() || !e.HasLatency() {
		return false
	}
	s := c.nodeState(h)
	if s.VerifiedIP != "" && s.VerifiedIP != e.GetEgressIP().String() {
		return false
	}
	verified := s.VerifiedNs
	if verified == 0 || e.LastEgressUpdate.Load() < verified {
		verified = e.LastEgressUpdate.Load()
	}
	return verified > 0 && time.Since(time.Unix(0, verified)) < age
}
func retryDelay(failures int) time.Duration {
	switch failures {
	case 1:
		return 5 * time.Minute
	case 2:
		return 30 * time.Minute
	case 3:
		return 2 * time.Hour
	default:
		return 12 * time.Hour
	}
}

func (c *MeteredController) run(m *ProbeManager, h node.Hash, reason string, maxAge ...time.Duration) (netip.Addr, egressProbeErrorStage, error) {
	if !c.begin() {
		return netip.Addr{}, egressProbeFetchError, fmt.Errorf("probe manager stopped")
	}
	defer c.active.Done()
	value, err, _ := c.flights.Do(h.Hex(), func() (any, error) {
		id, p, ok := c.Policy(h)
		if !ok {
			return nil, fmt.Errorf("metered node no longer enabled")
		}
		age := time.Duration(p.MaxEgressAge)
		if len(maxAge) > 0 && maxAge[0] > 0 && maxAge[0] < age {
			age = maxAge[0]
		}
		if reason != "manual" && c.Fresh(h, age) && (reason != "periodic" || !c.Due(h)) {
			e, _ := c.pool.GetEntry(h)
			return e.GetEgressIP(), nil
		}
		if reason != "manual" && c.nodeState(h).NextRetryNs > time.Now().UnixNano() {
			return nil, fmt.Errorf("node awaiting probe retry")
		}
		timer := time.NewTimer(c.slotWait)
		defer timer.Stop()
		if reason == "periodic" || reason == "retry" {
			select {
			case c.backgroundSlots <- struct{}{}:
				defer func() { <-c.backgroundSlots }()
			case <-timer.C:
				return nil, fmt.Errorf("probe concurrency limit; retry later")
			case <-m.stopCh:
				return nil, fmt.Errorf("probe manager stopped")
			}
		}
		select {
		case c.slots <- struct{}{}:
			defer func() { <-c.slots }()
		case <-timer.C:
			return nil, fmt.Errorf("probe concurrency limit; retry later")
		case <-m.stopCh:
			return nil, fmt.Errorf("probe manager stopped")
		}
		// Recheck policy after waiting; an operator can disable a subscription in the meantime.
		id, p, ok = c.Policy(h)
		if !ok {
			return nil, fmt.Errorf("metered node no longer enabled")
		}
		if reason == "periodic" && !c.Due(h) {
			e, _ := c.pool.GetEntry(h)
			return e.GetEgressIP(), nil
		}
		if reason == "periodic" && c.nodeState(h).Failures > 0 {
			reason = "retry"
		}
		if (p.StrictBudget || reason == "periodic" || reason == "retry") && c.budgetBlocked(id, p) {
			return nil, state.ErrProbeBudget
		}
		day := time.Now().UTC().Format("2006-01-02")
		if err := c.repo.ReserveProbe(id, day, reason, p.MonthlyBudgetBytes, ProbeTransferLimit, p.StrictBudget || reason == "periodic" || reason == "retry"); err != nil {
			if errors.Is(err, state.ErrProbeBudget) {
				c.mu.Lock()
				c.blocked[id] = budgetBlock{day[:7], p.MonthlyBudgetBytes, time.Now().Add(time.Minute)}
				c.mu.Unlock()
			}
			return nil, err
		}
		var ingress, egress int64
		fetch := func(h node.Hash, url string) ([]byte, time.Duration, error) {
			if m.statsFetcher == nil {
				return nil, 0, fmt.Errorf("metered stats fetcher unavailable")
			}
			body, latency, in, out, err := m.statsFetcher(h, url)
			ingress += in
			egress += out
			if err == nil {
				_, _, err = ParseCloudflareTrace(body)
			}
			return body, latency, err
		}
		if m.onProbeEvent != nil {
			m.onProbeEvent("egress")
		}
		ip, _, probeErr := m.performEgressProbeWithFetcher(h, fetch)
		if err := c.repo.FinishProbe(id, day, reason, ProbeTransferLimit, ingress, egress, probeErr != nil); err != nil {
			return nil, err
		}
		if err := c.update(h, func(s *state.ProbeNodeState) {
			if probeErr == nil {
				s.VerifiedNs = time.Now().UnixNano()
				s.VerifiedIP = ip.String()
				s.Failures = 0
				s.NextRetryNs = 0
			} else {
				s.Failures++
				s.NextRetryNs = time.Now().Add(retryDelay(s.Failures)).UnixNano()
			}
		}); err != nil {
			return nil, err
		}
		return ip, probeErr
	})
	if err != nil {
		return netip.Addr{}, egressProbeFetchError, err
	}
	return value.(netip.Addr), egressProbeNoError, nil
}

// PrepareRoute verifies a sticky node before reuse, or warms one unused candidate
// on allocation. Unknown IP/region never enters the normal routable view.
func (m *ProbeManager) PrepareRoute(plat *platform.Platform, current node.Hash) error {
	c := m.metered
	if c == nil {
		return nil
	}
	if current != node.Zero {
		if _, _, ok := c.Policy(current); ok {
			if err := m.VerifyPlatformNode(plat, current); err != nil {
				return err
			}
			return nil
		}
		if plat.View().Contains(current) {
			return nil
		}
	}
	type candidate struct {
		h    node.Hash
		used int64
	}
	candidates := []candidate{}
	lookup := c.pool.MakeSubLookup()
	c.pool.Range(func(h node.Hash, e *node.NodeEntry) bool {
		if _, _, ok := c.Policy(h); !ok || !e.HasOutbound() || e.IsDisabledBySubscriptions(lookup) || !e.MatchTagFilter(plat.RegexFilters, lookup) {
			return true
		}
		s := c.nodeState(h)
		if s.NextRetryNs > time.Now().UnixNano() {
			return true
		}
		// Known incompatible regions can be excluded without paying for a request.
		_, policy, _ := c.Policy(h)
		if c.Fresh(h, time.Duration(policy.MaxEgressAge)) && e.GetEgressRegion() != "" && !platform.MatchRegionFilter(e.GetEgressRegion(), plat.RegionFilters) {
			return true
		}
		candidates = append(candidates, candidate{h, s.LastUsedNs})
		return true
	})
	// Random tie breaking prevents parallel accounts from all selecting the same
	// newly imported node, while preferring nodes not yet allocated.
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].used < candidates[j].used })
	var lastErr error
	for i, candidate := range candidates {
		if i >= 3 {
			break
		}
		h := candidate.h
		if err := c.Touch(h); err != nil {
			return err
		}
		if err := m.VerifyPlatformNode(plat, h); err != nil {
			lastErr = err
			continue
		}
		c.pool.NotifyNodeDirty(h)
		if plat.View().Contains(h) {
			return nil
		}
	}
	if plat.View().Size() > 0 {
		return nil
	}
	return lastErr
}

func (m *ProbeManager) VerifyRouteNode(h node.Hash) error { return m.VerifyPlatformNode(nil, h) }
func (m *ProbeManager) VerifyPlatformNode(plat *platform.Platform, h node.Hash) error {
	c := m.metered
	if c == nil {
		return nil
	}
	_, p, ok := c.Policy(h)
	if !ok {
		return nil
	}
	if err := c.Touch(h); err != nil {
		return err
	}
	age := time.Duration(p.MaxEgressAge)
	if plat != nil && plat.EgressVerificationMaxAgeNs > 0 && time.Duration(plat.EgressVerificationMaxAgeNs) < age {
		age = time.Duration(plat.EgressVerificationMaxAgeNs)
	}
	if !c.Fresh(h, age) {
		_, _, err := c.run(m, h, "required", age)
		if err == nil && !c.Fresh(h, age) {
			_, _, err = c.run(m, h, "required", age)
			if err == nil && !c.Fresh(h, age) {
				return fmt.Errorf("node verification did not produce a usable route")
			}
		}
		return err
	}
	return nil
}

func (m *ProbeManager) ObservePassiveResult(h node.Hash, success bool) {
	c := m.metered
	if c == nil {
		return
	}
	if !c.begin() {
		return
	}
	defer c.active.Done()
	if _, _, ok := c.Policy(h); !ok {
		return
	}
	if success && c.nodeState(h).Failures == 0 {
		if err := c.Touch(h); err != nil {
			log.Printf("[probe] persist activity: %v", err)
		}
		return
	}
	if err := c.update(h, func(s *state.ProbeNodeState) {
		s.LastUsedNs = time.Now().UnixNano()
		if success {
			s.Failures = 0
			s.NextRetryNs = 0
		} else {
			s.Failures++
			s.NextRetryNs = time.Now().Add(retryDelay(s.Failures)).UnixNano()
		}
	}); err != nil {
		log.Printf("[probe] persist passive health: %v", err)
	}
}

func (c *MeteredController) budgetBlocked(id string, p probepolicy.ProbePolicy) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.blocked[id]
	return b.month == time.Now().UTC().Format("2006-01") && b.limit == p.MonthlyBudgetBytes && time.Now().Before(b.until)
}

func (c *MeteredController) begin() bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.stopped {
		return false
	}
	c.active.Add(1)
	return true
}
func (c *MeteredController) stop() {
	if c == nil {
		return
	}
	c.lifecycleMu.Lock()
	c.stopped = true
	c.lifecycleMu.Unlock()
	c.active.Wait()
}

func (c *MeteredController) disabledMetered(h node.Hash) bool {
	if c == nil || !c.pool.IsNodeDisabled(h) {
		return false
	}
	e, ok := c.pool.GetEntry(h)
	if !ok {
		return false
	}
	for _, id := range e.SubscriptionIDs() {
		if sub := c.subs.Lookup(id); sub != nil && sub.ProbePolicy().Mode == "metered" {
			return true
		}
	}
	return false
}
