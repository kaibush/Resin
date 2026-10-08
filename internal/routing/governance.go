package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

type governanceClient struct {
	endpoint, token string
	client          *http.Client
}

func newGovernanceClient() *governanceClient {
	endpoint := strings.TrimSpace(os.Getenv("RESIN_GOVERNANCE_URL"))
	if endpoint == "" {
		return nil
	}
	return &governanceClient{endpoint: endpoint, token: os.Getenv("RESIN_GOVERNANCE_TOKEN"), client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (r *Router) GovernanceConfigured() bool {
	return r != nil && r.governance != nil && r.governance.token != ""
}

type governanceCandidate struct {
	NodeHash string `json:"nodeHash"`
	IP       string `json:"ip"`
}
type governanceDecision struct {
	Mode              string    `json:"mode"`
	Status            string    `json:"status"`
	NodeHash          string    `json:"nodeHash"`
	IP                string    `json:"ip"`
	ValidUntil        time.Time `json:"validUntil"`
	FollowPlatformTTL bool      `json:"followPlatformTTL"`
}

func (r *Router) governanceDecision(plat *platform.Platform, account string, current Lease, loaded bool, checkOnly ...bool) (governanceDecision, error) {
	input := struct {
		CheckOnly       bool                  `json:"checkOnly"`
		PlatformID      string                `json:"platformID"`
		PlatformName    string                `json:"platformName"`
		Identity        string                `json:"identity"`
		CurrentIP       string                `json:"currentIP"`
		CurrentNodeHash string                `json:"currentNodeHash"`
		Candidates      []governanceCandidate `json:"candidates"`
	}{PlatformID: plat.ID, PlatformName: plat.Name, Identity: account, Candidates: []governanceCandidate{}}
	if loaded {
		input.CurrentIP = current.EgressIP.Unmap().String()
		input.CurrentNodeHash = current.NodeHash.Hex()
	}
	// Scan the complete allowed platform view before capacity selection. P2C or
	// repeated random sampling cannot prove that an under-limit IP is absent.
	if len(checkOnly) > 0 && checkOnly[0] {
		input.CheckOnly = true
	}
	if !input.CheckOnly {
		plat.View().Range(func(h node.Hash) bool {
			entry, ok := r.pool.GetEntry(h)
			if ok && entry.Outbound.Load() != nil && entry.GetEgressIP().IsValid() {
				input.Candidates = append(input.Candidates, governanceCandidate{h.Hex(), entry.GetEgressIP().Unmap().String()})
			}
			return len(input.Candidates) <= 20000
		})
	}
	if len(input.Candidates) > 20000 {
		return governanceDecision{}, fmt.Errorf("%w: governance candidate scan limit", ErrNoAvailableNodes)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return governanceDecision{}, err
	}
	req, err := http.NewRequest(http.MethodPost, r.governance.endpoint, bytes.NewReader(body))
	if err != nil {
		return governanceDecision{}, ErrNoAvailableNodes
	}
	req.Header.Set("Authorization", "Bearer "+r.governance.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := r.governance.client.Do(req)
	if err != nil {
		return governanceDecision{}, fmt.Errorf("%w: governance unavailable", ErrNoAvailableNodes)
	}
	defer response.Body.Close()
	var result governanceDecision
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil {
		return result, fmt.Errorf("%w: governance denied", ErrNoAvailableNodes)
	}
	if input.CheckOnly && (result.Mode == "soft" || result.Mode == "strict") && result.Status == "enforced" {
		return result, nil
	}
	if result.Mode == "audit" && result.Status == "audit" {
		return result, nil
	}
	if (result.Mode != "soft" && result.Mode != "strict") || result.Status != "allowed" || !result.ValidUntil.After(time.Now()) || result.ValidUntil.After(time.Now().Add(301*time.Second)) {
		return result, fmt.Errorf("%w: waiting for governed IP", ErrNoAvailableNodes)
	}
	return result, nil
}

func (r *Router) decideGovernedLease(plat *platform.Platform, state *PlatformRoutingState, account, target string, now time.Time, current Lease, loaded bool) (Lease, xsync.ComputeOp, RouteResult, error) {
	decision, err := r.governanceDecision(plat, account, current, loaded)
	if err != nil {
		return current, xsync.CancelOp, RouteResult{}, err
	}
	if decision.Mode == "audit" {
		return r.decideStickyLease(plat, state, account, target, now, now.UnixNano(), current, loaded)
	}
	hash, err := node.ParseHex(decision.NodeHash)
	ip, ipErr := netip.ParseAddr(decision.IP)
	if err != nil || ipErr != nil {
		return current, xsync.CancelOp, RouteResult{}, ErrNoAvailableNodes
	}
	if r.verifyNode != nil {
		if err := r.verifyNode(plat, hash); err != nil {
			return current, xsync.CancelOp, RouteResult{}, err
		}
	}
	entry, ok := r.pool.GetEntry(hash)
	if !ok || !plat.View().Contains(hash) || entry.GetEgressIP().Unmap() != ip.Unmap() || entry.Outbound.Load() == nil {
		return current, xsync.CancelOp, RouteResult{}, ErrNoAvailableNodes
	}
	expiry := decision.ValidUntil.UnixNano()
	// Admission validity is checked above on every request, independently of
	// how long the sticky account/IP relationship is retained.
	if decision.FollowPlatformTTL {
		ttl := time.Duration(plat.StickyTTLNs)
		if ttl <= 0 {
			ttl = 24 * time.Hour // Same safeguard as createLease.
		}
		expiry = now.Add(ttl).UnixNano()
	} else if plat.StickyTTLNs > 0 && now.Add(time.Duration(plat.StickyTTLNs)).UnixNano() < expiry {
		expiry = now.Add(time.Duration(plat.StickyTTLNs)).UnixNano()
	}
	lease := Lease{NodeHash: hash, EgressIP: ip.Unmap(), CreatedAtNs: now.UnixNano(), ExpiryNs: expiry, LastAccessedNs: now.UnixNano()}
	kind := LeaseCreate
	if loaded {
		state.IPLoadStats.Dec(current.EgressIP)
		kind = LeaseReplace
		if current.NodeHash == hash && current.EgressIP == lease.EgressIP {
			kind = LeaseTouch
			lease.CreatedAtNs = current.CreatedAtNs
		}
	}
	state.IPLoadStats.Inc(lease.EgressIP)
	r.emitLeaseEvent(LeaseEvent{Type: kind, PlatformID: plat.ID, Account: account, NodeHash: hash, EgressIP: lease.EgressIP})
	return lease, xsync.UpdateOp, RouteResult{Governed: true, NodeHash: hash, EgressIP: lease.EgressIP, LeaseCreated: kind != LeaseTouch}, nil
}

func (r *Router) GovernanceReady() bool {
	if !r.GovernanceConfigured() {
		return false
	}
	_, err := r.governanceDecision(&platform.Platform{ID: "__capability", Name: "__capability"}, "", Lease{}, false, true)
	return err == nil
}

func (r *Router) GovernanceEnforced() bool {
	if r == nil || r.governance == nil {
		return false
	}
	policy, err := r.governanceDecision(&platform.Platform{ID: "__capability", Name: "__capability"}, "", Lease{}, false, true)
	return err != nil || policy.Mode != "audit"
}
