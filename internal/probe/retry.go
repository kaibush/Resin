package probe

import (
	"regexp"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

// CONNECT errors come from the outbound transport as wrapped text, while
// direct HTTP status errors also include a status code. Match the status
// context rather than arbitrary occurrences of 429 in addresses or URLs.
var rateLimitStatus = regexp.MustCompile(`(?i)(?:unexpected status(?: code)?|status(?: code)?[:=]?|HTTP/\d(?:\.\d)?)\s+429\b`)

type retryState struct {
	failures int
	due      time.Time
}
type probeGroup struct {
	next     time.Time
	cooldown time.Time
	strikes  int
}

func (m *ProbeManager) failureRetryEnabled() bool {
	return m.retryEnabled != nil && m.retryEnabled()
}

func egressFailureRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 6 {
		return 30 * time.Minute
	}
	d := time.Minute * time.Duration(1<<(failures-1))
	if d > 30*time.Minute {
		return 30 * time.Minute
	}
	return d
}

func (m *ProbeManager) egressRetryDue(h node.Hash, lastAttempt int64) time.Time {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	if s, ok := m.retries[h]; ok {
		return s.due
	}
	// Recover failed attempts persisted by older versions or before restart.
	return time.Unix(0, lastAttempt).Add(time.Minute)
}

func (m *ProbeManager) recordEgressRetry(h node.Hash, success bool) {
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	if success || !m.failureRetryEnabled() {
		delete(m.retries, h)
		return
	}
	s := m.retries[h]
	if s.failures < 7 {
		s.failures++
	}
	// Stable per-node jitter prevents synchronized retries after an outage.
	s.due = time.Now().Add(egressFailureRetryDelay(s.failures) + time.Duration(h[0])*time.Second/16)
	m.retries[h] = s
}

// All automatic unmetered probes share a one-request/second subscription
// allowance. Admission never sleeps in a worker: skipped tasks remain due and
// are rediscovered by the scanner. Manual probes remain explicitly on demand.
func (m *ProbeManager) admitAutomaticProbe(e *node.NodeEntry, now time.Time) bool {
	if !m.failureRetryEnabled() {
		return true
	}
	ids := e.SubscriptionIDs()
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	for _, id := range ids {
		g := m.probeGroups[id]
		if now.Before(g.next) || now.Before(g.cooldown) {
			return false
		}
	}
	for _, id := range ids {
		g := m.probeGroups[id]
		g.next = now.Add(time.Second)
		m.probeGroups[id] = g
	}
	return true
}

func isRateLimited(err error) bool { return err != nil && rateLimitStatus.MatchString(err.Error()) }

// A subscription is the available grouping boundary. Accounts shared across
// subscriptions need upstream account limits as well; this does not throttle
// business requests. Concurrent failures within one cooldown count as one strike.
func (m *ProbeManager) noteRateLimit(h node.Hash, err error) bool {
	if !m.failureRetryEnabled() || !isRateLimited(err) {
		return false
	}
	e, ok := m.pool.GetEntry(h)
	if !ok {
		return true
	}
	now := time.Now()
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	for _, id := range e.SubscriptionIDs() {
		g := m.probeGroups[id]
		if now.Before(g.cooldown) {
			continue
		}
		if now.Sub(g.cooldown) > time.Hour {
			g.strikes = 0
		}
		if g.strikes < 7 {
			g.strikes++
		}
		g.cooldown = now.Add(egressFailureRetryDelay(g.strikes))
		m.probeGroups[id] = g
	}
	return true
}

// Drop state for removed nodes and inactive subscriptions without adding a
// cleanup goroutine. Only the egress scanner calls this method.
func (m *ProbeManager) pruneRetries(now time.Time) {
	if now.Sub(m.retrySweep) < time.Minute {
		return
	}
	m.retrySweep = now
	m.retryMu.Lock()
	defer m.retryMu.Unlock()
	for h := range m.retries {
		if _, ok := m.pool.GetEntry(h); !ok {
			delete(m.retries, h)
		}
	}
	for id, g := range m.probeGroups {
		if now.Sub(g.next) > time.Hour && now.Sub(g.cooldown) > time.Hour {
			delete(m.probeGroups, id)
		}
	}
}
