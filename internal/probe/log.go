package probe

import (
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/state"
)

// A bounded queue keeps diagnostic I/O off the routing/probing path.
type logWriter struct {
	mu      sync.Mutex
	closed  bool
	queue   chan state.ProbeLog
	done    chan struct{}
	dropped atomic.Int64
}

func newLogWriter(repo *state.StateRepo) *logWriter {
	if repo == nil {
		return nil
	}
	w := &logWriter{queue: make(chan state.ProbeLog, 8192), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		batch := make([]state.ProbeLog, 0, 256)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := repo.AppendProbeLogs(batch); err != nil {
				w.dropped.Add(int64(len(batch)))
				log.Printf("[probe] write diagnostic logs: %v", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case row, ok := <-w.queue:
				if !ok {
					flush()
					return
				}
				batch = append(batch, row)
				if len(batch) >= 256 {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}()
	return w
}
func (w *logWriter) emit(row state.ProbeLog) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.queue <- row:
	default:
		w.dropped.Add(1)
	}
}
func (w *logWriter) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
}
func (m *ProbeManager) DroppedProbeLogs() int64 {
	if m.logs == nil {
		return 0
	}
	return m.logs.dropped.Load()
}

func (m *ProbeManager) logRow(h node.Hash, target, kind, reason string) state.ProbeLog {
	row := state.ProbeLog{Ts: time.Now().UTC(), NodeHash: h.Hex(), Kind: kind, Reason: reason}
	if u, err := url.Parse(target); err == nil {
		row.TargetHost = u.Hostname()
	}
	if e, ok := m.pool.GetEntry(h); ok {
		ids := e.SubscriptionIDs()
		sort.Strings(ids)
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			if m.metered != nil {
				if s := m.metered.subs.Lookup(id); s != nil {
					names = append(names, s.Name()+" ("+id+")")
					continue
				}
			}
			names = append(names, id)
		}
		row.Subscriptions = strings.Join(names, ", ")
	}
	return row
}
func (m *ProbeManager) finishLog(row state.ProbeLog, err error) {
	row.DurationMs = time.Since(row.Ts).Milliseconds()
	row.Success = err == nil
	if err != nil {
		// Do not retain arbitrary upstream errors, which may contain URL credentials.
		row.Error = "probe failed"
		var netErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			row.Error = "probe canceled"
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			row.Error = "probe timed out"
		case errors.Is(err, syscall.ECONNREFUSED):
			row.Error = "connection refused"
		case errors.Is(err, syscall.ECONNRESET):
			row.Error = "connection reset"
		case strings.Contains(err.Error(), "transfer limit exceeded"):
			row.Error = "probe transfer limit exceeded"
		case strings.Contains(err.Error(), "response exceeds"):
			row.Error = "probe response too large"
		case strings.Contains(err.Error(), "redirect limit"):
			row.Error = "probe redirect limit exceeded"
		case strings.Contains(err.Error(), "unexpected status"):
			row.Error = "unexpected HTTP status"
		case strings.Contains(strings.ToLower(err.Error()), "parse") || strings.Contains(err.Error(), "trace"):
			row.Error = "invalid trace response"
		}

	}
	m.logs.emit(row)
}
func (m *ProbeManager) measuredFetcher(row *state.ProbeLog) Fetcher {
	return func(h node.Hash, target string) ([]byte, time.Duration, error) {
		if m.observedFetcher == nil {
			return m.fetcher(h, target)
		}
		body, latency, in, out, err := m.observedFetcher(h, target)
		row.IngressBytes += in
		row.EgressBytes += out
		row.BytesMeasured = true
		row.LatencyMs = latency.Milliseconds()
		return body, latency, err
	}
}
