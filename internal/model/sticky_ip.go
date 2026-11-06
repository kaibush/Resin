package model

import (
	"crypto/sha256"
	"net/netip"
)

// StickyIP remembers only the latest exit, without retaining account plaintext.
type StickyIP struct {
	PlatformID  string
	AccountHash [sha256.Size]byte
	EgressIP    netip.Addr
	UpdatedAtNs int64
}
