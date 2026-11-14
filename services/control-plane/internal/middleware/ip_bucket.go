package middleware

import (
	"net/netip"

	"github.com/gin-gonic/gin"
)

// unattributedIPBucket is deliberately a key identity rather than an address.
// A missing or malformed peer must not become an unbounded collection of raw
// strings in security-control counters.
const unattributedIPBucket = "unattributed"

// IPBucket returns the canonical pre-auth budget identity for a source address:
// IPv4 (including mapped IPv4) is a host bucket, while native IPv6 aggregates
// at /64. Invalid input has one stable unattributed bucket.
func IPBucket(raw string) string {
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return unattributedIPBucket
	}

	address = address.WithZone("").Unmap()
	if address.Is4() {
		return address.String()
	}
	prefix, err := address.Prefix(64)
	if err != nil {
		return unattributedIPBucket
	}
	return prefix.Masked().Addr().String()
}

// ClientIPBucket applies IPBucket to Gin's trusted-proxy-resolved client IP.
func ClientIPBucket(c *gin.Context) string {
	return IPBucket(c.ClientIP())
}
