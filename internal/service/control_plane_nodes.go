package service

import (
	"bytes"
	"cmp"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/probepolicy"
	"github.com/Resinat/Resin/internal/subscription"
)

// ------------------------------------------------------------------
// Nodes
// ------------------------------------------------------------------

// NodeFilters holds query filters for listing nodes.
type NodeFilters struct {
	PlatformID     *string
	SubscriptionID *string
	Enabled        *bool
	Region         *string
	CircuitOpen    *bool
	HasOutbound    *bool
	EgressIP       *string
	ProbedSince    *time.Time
	TagKeyword     *string
}

type NodeListOptions struct {
	SortBy    string
	SortOrder string
	Limit     int
	Offset    int
}

type NodeListPage struct {
	Items                  []NodeSummary `json:"items"`
	Total                  int           `json:"total"`
	Limit                  int           `json:"limit"`
	Offset                 int           `json:"offset"`
	UniqueEgressIPs        int           `json:"unique_egress_ips"`
	UniqueHealthyEgressIPs int           `json:"unique_healthy_egress_ips"`
}

type nodeListCandidate struct {
	hash         node.Hash
	entry        *node.NodeEntry
	sortKey      string
	failureCount int
}

// ListNodes returns nodes from the pool with optional filters.
func (s *ControlPlaneService) ListNodes(filters NodeFilters) ([]NodeSummary, error) {
	result := make([]NodeSummary, 0)
	err := s.forEachMatchingNodeEntry(filters, func(h node.Hash, entry *node.NodeEntry, _ node.SubLookupFunc) {
		result = append(result, s.nodeEntryToSummary(h, entry))
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *ControlPlaneService) ListNodesPage(filters NodeFilters, options NodeListOptions) (NodeListPage, error) {
	sortBy := options.SortBy
	switch sortBy {
	case "created_at", "failure_count", "region", "tag":
	default:
		sortBy = "tag"
	}

	candidates := make([]nodeListCandidate, 0)
	uniqueIPs := make(map[string]bool)
	err := s.forEachMatchingNodeEntry(filters, func(h node.Hash, entry *node.NodeEntry, subLookup node.SubLookupFunc) {
		candidate := nodeListCandidate{hash: h, entry: entry}
		switch sortBy {
		case "created_at":
			candidate.sortKey = entry.CreatedAt.UTC().Format(time.RFC3339Nano)
		case "failure_count":
			candidate.failureCount = int(entry.FailureCount.Load())
		case "region":
			if entry.GetEgressIP().IsValid() {
				candidate.sortKey = entry.GetRegion(s.geoIPLookup())
			}
		default:
			candidate.sortKey = s.nodeListTagSortKey(h, entry)
		}
		candidates = append(candidates, candidate)

		ip := entry.GetEgressIP()
		if !ip.IsValid() {
			return
		}
		key := ip.String()
		enabled := true
		if s != nil && s.Pool != nil {
			enabled = !entry.IsDisabledBySubscriptions(subLookup)
		}
		if enabled && entry.HasOutbound() && entry.CircuitOpenSince.Load() <= 0 {
			uniqueIPs[key] = true
		} else if _, ok := uniqueIPs[key]; !ok {
			uniqueIPs[key] = false
		}
	})
	if err != nil {
		return NodeListPage{}, err
	}

	descending := options.SortOrder == "desc"
	slices.SortFunc(candidates, func(a, b nodeListCandidate) int {
		order := 0
		if sortBy == "failure_count" {
			order = cmp.Compare(a.failureCount, b.failureCount)
		} else {
			order = strings.Compare(a.sortKey, b.sortKey)
		}
		if order == 0 {
			order = bytes.Compare(a.hash[:], b.hash[:])
		}
		if descending {
			return -order
		}
		return order
	})

	total := len(candidates)
	start := options.Offset
	if start > total {
		start = total
	}
	if start < 0 {
		start = 0
	}
	end := total
	if options.Limit < end-start {
		end = start + options.Limit
	}
	if end < start {
		end = start
	}

	items := make([]NodeSummary, 0, end-start)
	for _, candidate := range candidates[start:end] {
		items = append(items, s.nodeEntryToSummary(candidate.hash, candidate.entry))
	}

	uniqueHealthyIPs := 0
	for _, healthy := range uniqueIPs {
		if healthy {
			uniqueHealthyIPs++
		}
	}
	return NodeListPage{
		Items:                  items,
		Total:                  total,
		Limit:                  options.Limit,
		Offset:                 options.Offset,
		UniqueEgressIPs:        len(uniqueIPs),
		UniqueHealthyEgressIPs: uniqueHealthyIPs,
	}, nil
}

func (s *ControlPlaneService) forEachMatchingNodeEntry(
	filters NodeFilters,
	visit func(node.Hash, *node.NodeEntry, node.SubLookupFunc),
) error {
	var subLookup node.SubLookupFunc
	if s != nil && s.Pool != nil {
		subLookup = s.Pool.MakeSubLookup()
	}

	// If platform_id filter, get the platform view.
	var platformView map[node.Hash]struct{}
	if filters.PlatformID != nil {
		plat, ok := s.Pool.GetPlatform(*filters.PlatformID)
		if !ok {
			return notFound("platform not found")
		}
		platformView = make(map[node.Hash]struct{}, plat.View().Size())
		plat.View().Range(func(h node.Hash) bool {
			platformView[h] = struct{}{}
			return true
		})
	}

	var subNodes map[node.Hash]struct{}
	if filters.SubscriptionID != nil {
		sub := s.SubMgr.Lookup(*filters.SubscriptionID)
		if sub == nil {
			return notFound("subscription not found")
		}
		subNodes = make(map[node.Hash]struct{})
		sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
			if managed.Evicted {
				return true
			}
			subNodes[h] = struct{}{}
			return true
		})
	}

	appendIfMatched := func(h node.Hash, entry *node.NodeEntry) {
		if !s.nodeEntryMatchesFilters(entry, filters, subLookup) {
			return
		}
		visit(h, entry, subLookup)
	}

	appendIfMatchedHash := func(h node.Hash) {
		entry, ok := s.Pool.GetEntry(h)
		if !ok {
			return
		}
		appendIfMatched(h, entry)
	}

	switch {
	case platformView != nil && subNodes != nil:
		// Iterate the smaller candidate set, then intersect by membership.
		if len(platformView) <= len(subNodes) {
			for h := range platformView {
				if _, ok := subNodes[h]; !ok {
					continue
				}
				appendIfMatchedHash(h)
			}
		} else {
			for h := range subNodes {
				if _, ok := platformView[h]; !ok {
					continue
				}
				appendIfMatchedHash(h)
			}
		}
	case platformView != nil:
		for h := range platformView {
			appendIfMatchedHash(h)
		}
	case subNodes != nil:
		for h := range subNodes {
			appendIfMatchedHash(h)
		}
	default:
		s.Pool.Range(func(h node.Hash, entry *node.NodeEntry) bool {
			appendIfMatched(h, entry)
			return true
		})
	}

	return nil
}

func (s *ControlPlaneService) geoIPLookup() func(netip.Addr) string {
	if s == nil || s.GeoIP == nil {
		return nil
	}
	return s.GeoIP.Lookup
}

func (s *ControlPlaneService) nodeListTagSortKey(h node.Hash, entry *node.NodeEntry) string {
	if s != nil && s.Pool != nil {
		if displayTag := s.Pool.ResolveNodeDisplayTag(h); displayTag != "" {
			return displayTag
		}
	}

	if s == nil || s.SubMgr == nil {
		return ""
	}
	bestFound := false
	var bestCreatedAtNs int64
	bestTag := ""
	for _, subID := range entry.SubscriptionIDs() {
		sub := s.SubMgr.Lookup(subID)
		if sub == nil {
			continue
		}
		managed, ok := sub.ManagedNodes().LoadNode(h)
		if !ok {
			continue
		}
		for _, tag := range managed.Tags {
			displayTag := sub.Name() + "/" + tag
			if !bestFound || sub.CreatedAtNs < bestCreatedAtNs ||
				(sub.CreatedAtNs == bestCreatedAtNs && (bestTag == "" || displayTag < bestTag)) {
				bestFound = true
				bestCreatedAtNs = sub.CreatedAtNs
				bestTag = displayTag
			}
		}
	}
	return bestTag
}

func (s *ControlPlaneService) nodeEntryMatchesFilters(
	entry *node.NodeEntry,
	filters NodeFilters,
	subLookup node.SubLookupFunc,
) bool {
	// Enabled/disabled filter.
	if filters.Enabled != nil {
		enabled := true
		if subLookup != nil {
			enabled = entry.HasEnabledSubscription(subLookup)
		}
		if enabled != *filters.Enabled {
			return false
		}
	}

	// Node tag fuzzy search filter.
	if filters.TagKeyword != nil {
		keyword := strings.ToLower(strings.TrimSpace(*filters.TagKeyword))
		if keyword != "" {
			matched := false
			for _, subID := range entry.SubscriptionIDs() {
				sub := s.SubMgr.Lookup(subID)
				if sub == nil {
					continue
				}
				managed, ok := sub.ManagedNodes().LoadNode(entry.Hash)
				if !ok {
					continue
				}
				tags := managed.Tags
				for _, tag := range tags {
					displayTag := sub.Name() + "/" + tag
					if strings.Contains(strings.ToLower(displayTag), keyword) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		}
	}

	// Region filter.
	if filters.Region != nil {
		region := entry.GetRegion(nil)
		if s.GeoIP != nil {
			region = entry.GetRegion(s.GeoIP.Lookup)
		}
		if region == "" || region != *filters.Region {
			return false
		}
	}
	// Circuit open filter.
	if filters.CircuitOpen != nil {
		if entry.IsCircuitOpen() != *filters.CircuitOpen {
			return false
		}
	}
	// Has outbound filter.
	if filters.HasOutbound != nil {
		if entry.HasOutbound() != *filters.HasOutbound {
			return false
		}
	}
	// Egress IP filter.
	if filters.EgressIP != nil {
		egressIP := entry.GetEgressIP()
		if !egressIP.IsValid() || egressIP.String() != *filters.EgressIP {
			return false
		}
	}
	// Probed since filter.
	if filters.ProbedSince != nil {
		lastUpdate := entry.LastLatencyProbeAttempt.Load()
		if lastUpdate < filters.ProbedSince.UnixNano() {
			return false
		}
	}
	return true
}

// GetNode returns a single node by hash.
func (s *ControlPlaneService) GetNode(hashStr string) (*NodeSummary, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	entry, ok := s.Pool.GetEntry(h)
	if !ok {
		return nil, notFound("node not found")
	}
	ns := s.nodeEntryToSummary(h, entry)
	return &ns, nil
}

// ProbeEgress triggers a synchronous egress probe and returns results.
func (s *ControlPlaneService) ProbeEgress(hashStr string) (*probe.EgressProbeResult, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	entry, ok := s.Pool.GetEntry(h)
	if !ok {
		return nil, notFound("node not found")
	}
	result, err := s.ProbeMgr.ProbeEgressSync(h)
	if err != nil {
		if errors.Is(err, probepolicy.ErrBudget) {
			return nil, conflict("probe traffic budget exhausted")
		}
		return nil, internal("egress probe failed", err)
	}
	result.Region = entry.GetRegion(nil)
	if s.GeoIP != nil {
		result.Region = entry.GetRegion(s.GeoIP.Lookup)
	}
	return result, nil
}

// ProbeLatency triggers a synchronous latency probe and returns results.
func (s *ControlPlaneService) ProbeLatency(hashStr string) (*probe.LatencyProbeResult, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	if _, ok := s.Pool.GetEntry(h); !ok {
		return nil, notFound("node not found")
	}
	result, err := s.ProbeMgr.ProbeLatencySync(h)
	if err != nil {
		if errors.Is(err, probepolicy.ErrBudget) {
			return nil, conflict("probe traffic budget exhausted")
		}
		return nil, internal("latency probe failed", err)
	}
	return result, nil
}
