package routing

import (
	"crypto/sha256"

	"github.com/puzpuzpuz/xsync/v4"
)

// PlatformRoutingState encapsulates the routing state for a single platform.
// This struct is stored in the Router's state map.
type PlatformRoutingState struct {
	Leases      *LeaseTable
	IPLoadStats *IPLoadStats
	stickyIPs   *xsync.Map[[sha256.Size]byte, stickyIPBinding]
}

// NewPlatformRoutingState creates a new state instance.
func NewPlatformRoutingState() *PlatformRoutingState {
	stats := NewIPLoadStats()
	return &PlatformRoutingState{
		Leases:      NewLeaseTable(stats),
		IPLoadStats: stats,
		stickyIPs:   xsync.NewMap[[sha256.Size]byte, stickyIPBinding](),
	}
}
