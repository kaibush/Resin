package config

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	// DefaultProbeConcurrency is the initial probe worker count, sized for
	// about 0.75 core. The live value is the persisted runtime setting.
	DefaultProbeConcurrency = 8
	// MaxProbeConcurrency is the inclusive upper bound for probe workers.
	MaxProbeConcurrency = 10000
	// DefaultProbeTimeout is the initial per-probe timeout.
	DefaultProbeTimeout = 15 * time.Second
	// MaxProbeTimeout is the inclusive upper bound for the runtime probe timeout.
	MaxProbeTimeout = 2 * time.Minute
)

// RuntimeConfig holds all hot-updatable global settings.
// These are persisted in the database and served via GET /system/config.
type RuntimeConfig struct {
	// Request log
	RequestLogEnabled                  bool `json:"request_log_enabled"`
	ReverseProxyLogDetailEnabled       bool `json:"reverse_proxy_log_detail_enabled"`
	ReverseProxyLogReqHeadersMaxBytes  int  `json:"reverse_proxy_log_req_headers_max_bytes"`
	ReverseProxyLogReqBodyMaxBytes     int  `json:"reverse_proxy_log_req_body_max_bytes"`
	ReverseProxyLogRespHeadersMaxBytes int  `json:"reverse_proxy_log_resp_headers_max_bytes"`
	ReverseProxyLogRespBodyMaxBytes    int  `json:"reverse_proxy_log_resp_body_max_bytes"`

	// Health check
	MaxConsecutiveFailures          int      `json:"max_consecutive_failures"`
	MaxLatencyTestInterval          Duration `json:"max_latency_test_interval"`
	MaxAuthorityLatencyTestInterval Duration `json:"max_authority_latency_test_interval"`
	MaxEgressTestInterval           Duration `json:"max_egress_test_interval"`

	// Probe
	ProbeFailureRetryEnabled bool     `json:"probe_failure_retry_enabled"`
	ProbeConcurrency         int      `json:"probe_concurrency"`
	ProbeTimeout             Duration `json:"probe_timeout"`
	LatencyTestURL           string   `json:"latency_test_url"`
	LatencyAuthorities       []string `json:"latency_authorities"`

	// P2C
	P2CLatencyWindow   Duration `json:"p2c_latency_window"`
	LatencyDecayWindow Duration `json:"latency_decay_window"`

	// Persistence
	CacheFlushInterval       Duration `json:"cache_flush_interval"`
	CacheFlushDirtyThreshold int      `json:"cache_flush_dirty_threshold"`
}

// NewDefaultRuntimeConfig returns a RuntimeConfig populated with the default
// values specified in DESIGN.md §运行时全局设置项.
func NewDefaultRuntimeConfig() *RuntimeConfig {
	return &RuntimeConfig{
		RequestLogEnabled:                  true,
		ReverseProxyLogDetailEnabled:       false,
		ReverseProxyLogReqHeadersMaxBytes:  4096,
		ReverseProxyLogReqBodyMaxBytes:     1024,
		ReverseProxyLogRespHeadersMaxBytes: 1024,
		ReverseProxyLogRespBodyMaxBytes:    1024,

		MaxConsecutiveFailures:          3,
		MaxLatencyTestInterval:          Duration(1 * time.Hour),
		MaxAuthorityLatencyTestInterval: Duration(3 * time.Hour),
		MaxEgressTestInterval:           Duration(24 * time.Hour),

		ProbeFailureRetryEnabled: true,
		ProbeConcurrency:         DefaultProbeConcurrency,
		ProbeTimeout:             Duration(DefaultProbeTimeout),
		LatencyTestURL:           "https://www.gstatic.com/generate_204",
		LatencyAuthorities:       []string{"gstatic.com", "google.com", "cloudflare.com", "github.com"},

		P2CLatencyWindow:   Duration(10 * time.Minute),
		LatencyDecayWindow: Duration(10 * time.Minute),

		CacheFlushInterval:       Duration(5 * time.Minute),
		CacheFlushDirtyThreshold: 1000,
	}
}

// ApplyMissingProbeSettings fills probe concurrency and timeout from the
// environment only when the persisted JSON does not already contain those keys.
// Values above the runtime limits are clamped. A true result means clamping
// happened. Explicit saved values are left unchanged.
func ApplyMissingProbeSettings(cfg *RuntimeConfig, rawJSON []byte, concurrency int, timeout time.Duration) bool {
	if cfg == nil {
		return false
	}
	present := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(rawJSON)) > 0 {
		if err := json.Unmarshal(rawJSON, &present); err != nil {
			return false
		}
	}
	clamped := false
	if _, ok := present["probe_concurrency"]; !ok && concurrency > 0 {
		if concurrency > MaxProbeConcurrency {
			concurrency = MaxProbeConcurrency
			clamped = true
		}
		cfg.ProbeConcurrency = concurrency
	}
	if _, ok := present["probe_timeout"]; !ok && timeout > 0 {
		if timeout > MaxProbeTimeout {
			timeout = MaxProbeTimeout
			clamped = true
		}
		cfg.ProbeTimeout = Duration(timeout)
	}
	return clamped
}
