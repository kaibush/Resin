package model

// IPHistory records the first observed use of an exit by a distinct sticky
// marker. It is independent of leases and survives expiry and pool changes.
// Only a SHA-256 digest of the marker is persisted.
type IPHistory struct {
	EgressIP    string
	AccountHash string
	FirstSeenNs int64
}
