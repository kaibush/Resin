package service

import (
	"net/netip"
	"strings"

	"github.com/Resinat/Resin/internal/routing"
)

type IPHistoryEntry struct {
	routing.IPHistoryLoad
	ThresholdStatus string `json:"threshold_status"`
}

type IPHistorySummary struct {
	AvailableIPs int  `json:"available_ips"`
	UsedIPs      int  `json:"used_ips"`
	UnusedIPs    int  `json:"unused_ips"`
	ActiveIPs    int  `json:"active_ips"`
	ReachedIPs   int  `json:"reached_ips"`
	Enabled      bool `json:"enabled"`
	SoftLimit    int  `json:"soft_limit"`
}

func (s *ControlPlaneService) GetIPHistory(platformID string) ([]IPHistoryEntry, IPHistorySummary, error) {
	p, ok := s.Pool.GetPlatform(platformID)
	if !ok {
		return nil, IPHistorySummary{}, notFound("platform not found")
	}
	loads, err := s.Router.SnapshotIPHistory(platformID)
	if err != nil {
		return nil, IPHistorySummary{}, err
	}
	summary := IPHistorySummary{AvailableIPs: len(loads), Enabled: p.IPGovernanceEnabled, SoftLimit: p.IPGovernanceMaxAccountsPerIP}
	entries := make([]IPHistoryEntry, 0, len(loads))
	for _, load := range loads {
		status := "below"
		if load.HistoricalAccounts == 0 {
			summary.UnusedIPs++
		} else {
			summary.UsedIPs++
		}
		if load.ActiveAccounts > 0 {
			summary.ActiveIPs++
		}
		switch {
		case !p.IPGovernanceEnabled:
			status = "disabled"
		case p.IPGovernanceMaxAccountsPerIP == 0:
			status = "unlimited"
		case load.HistoricalAccounts >= int64(p.IPGovernanceMaxAccountsPerIP):
			status = "reached"
			summary.ReachedIPs++
		}
		entries = append(entries, IPHistoryEntry{load, status})
	}
	return entries, summary, nil
}

func (s *ControlPlaneService) ResetIPHistory(platformID, address string) error {
	if _, ok := s.Pool.GetPlatform(platformID); !ok {
		return notFound("platform not found")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || ip.Zone() != "" {
		return invalidArg("egress_ip: must be a valid IP address")
	}
	return s.Router.ResetIPHistory(ip)
}
