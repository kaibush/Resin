package routing

// governsPlatform reports whether the platform opted into Resin's local
// per-IP sticky-lease ceiling. Allocation uses Resin's in-memory lease
// counters; it does not call back into grok2api.
func (r *Router) governsPlatform(id string) bool {
	if r == nil || r.pool == nil {
		return false
	}
	p, ok := r.pool.GetPlatform(id)
	return ok && p.IPGovernanceEnabled
}

// GovernanceEnforced prevents configured proxy-bypass rules from routing
// around a platform's local IP balancing policy.
func (r *Router) GovernanceEnforced(platformName string) bool {
	if r == nil {
		return false
	}
	p, err := r.resolvePlatform(platformName)
	if err != nil {
		return true
	}
	return r.governsPlatform(p.ID)
}
