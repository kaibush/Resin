package netutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const defaultOutboundUserAgent = "Resin/1.0"

type ConnLifecycleOp uint8

const (
	ConnLifecycleOpen ConnLifecycleOp = iota
	ConnLifecycleClose
)

// OutboundHTTPOptions controls outbound-backed HTTP execution behavior.
type OutboundHTTPOptions struct {
	MaxBodyBytes     int64
	MaxTransferBytes int64
	MaxRedirects     int
	// OnBytes reports bytes at the outbound stream, including destination TLS.
	OnBytes func(ingress, egress int64)

	// RequireStatusOK enforces HTTP 200 status; otherwise any status is accepted.
	RequireStatusOK bool
	// UserAgent overrides the request User-Agent when non-empty.
	UserAgent string
	// OnConnLifecycle is called with open/close lifecycle events to track connection
	// lifecycle for metrics. Set by probe callers to count outbound connections;
	// left nil for download callers (GeoIP, subscription) to exclude from stats.
	OnConnLifecycle func(op ConnLifecycleOp)
}

// HTTPGetViaOutbound executes an HTTP GET through the provided outbound.
// Timeout and cancellation are controlled solely by ctx.
func HTTPGetViaOutbound(
	ctx context.Context,
	outbound adapter.Outbound,
	url string,
	opts OutboundHTTPOptions,
) ([]byte, time.Duration, error) {
	if outbound == nil {
		return nil, 0, fmt.Errorf("outbound fetch: outbound is nil")
	}

	var ingress, egress, readAllowance, writeAllowance atomic.Int64
	// Separate directional allowances prevent a blocked Read from reserving the
	// bytes needed to send the request. Both are shared across redirects.
	writeAllowance.Store(opts.MaxTransferBytes / 4)
	readAllowance.Store(opts.MaxTransferBytes - opts.MaxTransferBytes/4)
	var connMu sync.Mutex
	var counted []*probeCountingConn
	finished := false
	defer func() {
		connMu.Lock()
		finished = true
		connections := append([]*probeCountingConn(nil), counted...)
		connMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		if opts.OnBytes != nil {
			opts.OnBytes(ingress.Load(), egress.Load())
		}
	}()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := outbound.DialContext(ctx, network, M.ParseSocksaddr(addr))
			if err != nil {
				return nil, err
			}
			if opts.OnBytes != nil || opts.MaxTransferBytes > 0 {
				countedConn := &probeCountingConn{Conn: conn, ingress: &ingress, egress: &egress, readAllowance: &readAllowance, writeAllowance: &writeAllowance, limited: opts.MaxTransferBytes > 0}
				connMu.Lock()
				if finished {
					connMu.Unlock()
					conn.Close()
					return nil, context.Canceled
				}
				counted = append(counted, countedConn)
				connMu.Unlock()
				conn = countedConn
			}
			if opts.OnConnLifecycle != nil {
				opts.OnConnLifecycle(ConnLifecycleOpen)
				return &connCloseHook{Conn: conn, onClose: func() { opts.OnConnLifecycle(ConnLifecycleClose) }}, nil
			}
			return conn, nil
		},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}

	client := &http.Client{
		Transport: transport,
	}

	defer transport.CloseIdleConnections()
	if opts.MaxRedirects > 0 {
		client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return fmt.Errorf("probe redirect limit exceeded")
			}
			return nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultOutboundUserAgent
	}
	req.Header.Set("User-Agent", userAgent)

	var start time.Time
	var latency time.Duration
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { start = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				latency = time.Since(start)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if opts.RequireStatusOK && resp.StatusCode != http.StatusOK {
		return nil, latency, fmt.Errorf("outbound fetch: unexpected status %d from %s", resp.StatusCode, url)
	}

	var reader io.Reader = resp.Body
	if opts.MaxBodyBytes > 0 {
		reader = io.LimitReader(reader, opts.MaxBodyBytes+1)
	}
	body, err := io.ReadAll(reader)
	if opts.MaxBodyBytes > 0 && int64(len(body)) > opts.MaxBodyBytes {
		return nil, latency, fmt.Errorf("probe response exceeds %d bytes", opts.MaxBodyBytes)
	}
	if err != nil {
		return nil, latency, err
	}

	return body, latency, nil
}

// connCloseHook wraps a net.Conn and calls onClose exactly once on Close.
type connCloseHook struct {
	net.Conn
	onClose   func()
	closeOnce sync.Once
	closeErr  error
}

func (c *connCloseHook) Close() error {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

// A shared allowance bounds transfer across redirect connections. Reserve before
// I/O without holding a lock during Read/Write (TLS can read and write concurrently).
type probeCountingConn struct {
	net.Conn
	ingress, egress, readAllowance, writeAllowance *atomic.Int64
	mu                                             sync.Mutex
	closed                                         bool
	ioWG                                           sync.WaitGroup
	closeOnce                                      sync.Once
	closeErr                                       error
	limited                                        bool
}

func (c *probeCountingConn) transfer(p []byte, read bool) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.ioWG.Add(1)
	c.mu.Unlock()
	defer c.ioWG.Done()
	allowance := c.writeAllowance
	if read {
		allowance = c.readAllowance
	}
	size := len(p)
	if size == 0 {
		return 0, nil
	}
	if c.limited {
		for {
			left := allowance.Load()
			if left <= 0 {
				return 0, fmt.Errorf("probe transfer limit exceeded")
			}
			n := int64(size)
			if n > left {
				n = left
			}
			if allowance.CompareAndSwap(left, left-n) {
				size = int(n)
				break
			}
		}
	}
	var n int
	var err error
	if read {
		n, err = c.Conn.Read(p[:size])
		c.ingress.Add(int64(n))
	} else {
		n, err = c.Conn.Write(p[:size])
		c.egress.Add(int64(n))
	}
	if c.limited {
		allowance.Add(int64(size - n))
	}
	if !read && n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}
func (c *probeCountingConn) Read(p []byte) (int, error)  { return c.transfer(p, true) }
func (c *probeCountingConn) Write(p []byte) (int, error) { return c.transfer(p, false) }

func (c *probeCountingConn) Close() error {
	c.closeOnce.Do(func() { c.mu.Lock(); c.closed = true; c.mu.Unlock(); c.closeErr = c.Conn.Close(); c.ioWG.Wait() })
	return c.closeErr
}
