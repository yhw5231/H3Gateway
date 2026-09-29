// Package identity mints the throw-away trial identities used to talk to the
// upstream anonymous channel.
//
// Upstream keys its two-generations-per-day anonymous allowance on the first
// token of the X-Forwarded-For header. Live probing (see internal/xffprobe)
// established three facts this package relies on:
//
//  1. The XFF value is trusted verbatim; the caller decides the "IP".
//  2. Both IPv4 and IPv6 literals are accepted and counted independently.
//  3. The value is compared as a raw string with only surrounding whitespace
//     trimmed, so equivalent textual forms of the same address (compressed vs
//     expanded, upper vs lower case) land in different buckets.
//
// Point 3 is what the optional variant spelling exploits.
package identity

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/model"
)

// Pool policies.
const (
	// PoolPublic mints globally routable addresses, like the Python original.
	PoolPublic = "public"
	// PoolReserved mints addresses from the RFC 5737 / RFC 3849 documentation
	// ranges. Upstream accepts them, and no real client shares those buckets.
	PoolReserved = "reserved"
)

// Identity is one forged trial identity. Upstream treats (XFF value, client id)
// as a fresh anonymous user.
type Identity struct {
	ClientID  string `json:"client_id"`
	VisitorID string `json:"visitor_id"`
	ForgedIP  string `json:"forged_ip"`
	Family    string `json:"family"`
	// UsesLeft tracks the remaining generations for ForgedIP. Upstream grants
	// two per XFF value per day.
	UsesLeft int `json:"uses_left"`
}

// Mint creates a brand new identity for the requested family.
//
// family must be model.FamilyIPv4 or model.FamilyIPv6; an empty family yields an
// identity without a forged XFF, which is what happens when forging is disabled.
func Mint(family, poolPolicy string, variants bool) *Identity {
	id := &Identity{
		ClientID:  "mmtrial_" + auth.GenerateID(),
		VisitorID: "mmguest_" + auth.GenerateID(),
		Family:    family,
		UsesLeft:  2,
	}
	switch family {
	case model.FamilyIPv6:
		id.ForgedIP = RandomIPv6(poolPolicy, variants)
	case model.FamilyIPv4:
		id.ForgedIP = RandomIPv4(poolPolicy)
	default:
		id.Family = model.FamilyNone
		id.UsesLeft = 1 << 30 // no per-IP quota to exhaust
	}
	return id
}

// ---------------------------------------------------------------------------
// address generation
// ---------------------------------------------------------------------------

// ipv4Excluded lists the blocks that are never valid global unicast space.
var ipv4Excluded = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
)

// ipv4Documentation are the RFC 5737 ranges kept for the "reserved" pool.
var ipv4Documentation = mustPrefixes(
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24",
)

// ipv6Excluded lists special-purpose blocks inside 2000::/3.
var ipv6Excluded = mustPrefixes(
	"2001:0::/32",   // Teredo
	"2001:2::/48",   // benchmarking
	"2001:10::/28",  // ORCHID
	"2001:20::/28",  // ORCHIDv2
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4
	"3ffe::/16",     // retired 6bone
)

// ipv6Documentation is the RFC 3849 range kept for the "reserved" pool.
var ipv6Documentation = mustPrefixes("2001:db8::/32")

// RandomIPv4 returns a random IPv4 literal.
func RandomIPv4(poolPolicy string) string {
	if poolPolicy == PoolReserved {
		return randomInIPv4Pool(ipv4Documentation)
	}
	for i := 0; i < 10000; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return fallbackIPv4()
		}
		addr := netip.AddrFrom4(b)
		if !addr.IsGlobalUnicast() || addr.IsPrivate() {
			continue
		}
		if inAnyPrefix(addr, ipv4Excluded) {
			continue
		}
		return addr.String()
	}
	return fallbackIPv4()
}

// RandomIPv6 returns a random IPv6 literal from the global unicast space.
//
// variants enables alternative spellings of the same address, which upstream
// counts as separate quota buckets.
func RandomIPv6(poolPolicy string, variants bool) string {
	var addr netip.Addr
	if poolPolicy == PoolReserved {
		addr = randomInIPv6Pool(ipv6Documentation)
	} else {
		addr = randomGlobalIPv6()
	}
	if variants {
		return Variant(addr.String())
	}
	return addr.String()
}

func randomGlobalIPv6() netip.Addr {
	for i := 0; i < 100000; i++ {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return netip.MustParseAddr("2001:db8::1")
		}
		// Constrain the first hextet to 0x2000-0x3fff, i.e. 2000::/3.
		b[0] = 0x20 | (b[0] & 0x1f)
		addr := netip.AddrFrom16(b)
		if inAnyPrefix(addr, ipv6Excluded) {
			continue
		}
		if !addr.IsGlobalUnicast() {
			continue
		}
		return addr
	}
	return netip.MustParseAddr("2001:db8::1")
}

func randomInIPv4Pool(pfx []netip.Prefix) string {
	for i := 0; i < 10000; i++ {
		p := pfx[int(binary.BigEndian.Uint32(randBytes(4)))%len(pfx)]
		if addr, ok := randomAddrIn(p); ok {
			return addr.String()
		}
	}
	return "192.0.2.1"
}

func randomInIPv6Pool(pfx []netip.Prefix) netip.Addr {
	for i := 0; i < 10000; i++ {
		p := pfx[int(binary.BigEndian.Uint32(randBytes(4)))%len(pfx)]
		if addr, ok := randomAddrIn(p); ok {
			return addr
		}
	}
	return netip.MustParseAddr("2001:db8::1")
}

// randomAddrIn picks a uniformly random address inside p, keeping the network
// prefix intact.
func randomAddrIn(p netip.Prefix) (netip.Addr, bool) {
	p = p.Masked()
	bits := p.Bits()
	addr := p.Addr()

	if addr.Is4() {
		raw := addr.As4()
		hostBits := 32 - bits
		for i := 0; i < hostBits; i++ {
			byteIdx := 3 - i/8
			bitIdx := uint(i % 8)
			if randBit() {
				raw[byteIdx] |= 1 << bitIdx
			} else {
				raw[byteIdx] &^= 1 << bitIdx
			}
		}
		a := netip.AddrFrom4(raw)
		if a.IsUnspecified() {
			return a, false
		}
		return a, true
	}

	raw := addr.As16()
	hostBits := 128 - bits
	for i := 0; i < hostBits; i++ {
		byteIdx := 15 - i/8
		bitIdx := uint(i % 8)
		if randBit() {
			raw[byteIdx] |= 1 << bitIdx
		} else {
			raw[byteIdx] &^= 1 << bitIdx
		}
	}
	return netip.AddrFrom16(raw), true
}

// Variant returns an alternative but still valid spelling of an IPv6 literal.
// Equivalent spellings occupy different upstream quota buckets because the raw
// header string is used as the key.
func Variant(canonical string) string {
	addr, err := netip.ParseAddr(canonical)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return canonical
	}
	expanded := expandIPv6(addr)
	switch pickVariant() {
	case 0:
		return strings.ToUpper(canonical)
	case 1:
		return expanded
	case 2:
		return strings.ToUpper(expanded)
	default:
		return expanded
	}
}

// expandIPv6 renders the full eight-group, zero-padded form.
func expandIPv6(a netip.Addr) string {
	b := a.As16()
	var sb strings.Builder
	sb.Grow(39)
	for i := 0; i < 8; i++ {
		if i > 0 {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%02x%02x", b[i*2], b[i*2+1])
	}
	return sb.String()
}

// pickVariant returns 0..2 with equal probability. Variant index 3 is folded
// into index 1 so the expanded form is favoured slightly.
func pickVariant() int {
	return int(randBytes(1)[0]) % 3
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func inAnyPrefix(a netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (8 * (i % 8)))
		}
	}
	return b
}

func randBit() bool { return randBytes(1)[0]&1 == 1 }

func fallbackIPv4() string { return "192.0.2.1" }

// ---------------------------------------------------------------------------
// rotator
// ---------------------------------------------------------------------------

// Rotator mints the throw-away identities used to talk to upstream.
//
// Identities are never recycled. Upstream ties the uploaded image and the task
// it creates to the identity that submitted them, so a reused identity makes a
// later generation answer with the earlier image ("changing the picture makes no
// difference") or hand back the earlier task ("the next request returns the
// previous video"). Forged addresses are effectively unlimited — IPv6 alone
// offers ~2^61 usable values — so reusing one buys nothing and risks exactly
// those two failures.
type Rotator struct {
	mu        sync.Mutex
	minted    int64
	exhausted int64
}

// NewRotator returns an empty rotator.
func NewRotator() *Rotator { return &Rotator{} }

// Mint creates a fresh identity and counts it.
func (r *Rotator) Mint(family, poolPolicy string, variants bool) *Identity {
	r.mu.Lock()
	r.minted++
	r.mu.Unlock()
	return Mint(family, poolPolicy, variants)
}

// Retire marks an identity as spent. Identities are retired after the single
// generation they were minted for, and immediately when upstream rejects them.
func (r *Rotator) Retire(id *Identity) {
	if id == nil {
		return
	}
	id.UsesLeft = 0
	r.mu.Lock()
	r.exhausted++
	r.mu.Unlock()
}

// Stats reports rotator counters for the dashboard.
type Stats struct {
	IdentitiesMinted    int64  `json:"identities_minted"`
	IdentitiesExhausted int64  `json:"identities_exhausted"`
	ProxyMode           bool   `json:"proxy_mode"`
	EffectiveXFFMode    string `json:"effective_xff_mode"`
	ForgedHeaderSent    bool   `json:"forged_header_sent"`
}

// Stats snapshots the counters.
func (r *Rotator) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{
		IdentitiesMinted:    r.minted,
		IdentitiesExhausted: r.exhausted,
	}
}

// Reset clears the lifetime counters, used by the admin console.
func (r *Rotator) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.minted = 0
	r.exhausted = 0
}

// ---------------------------------------------------------------------------
// mode resolution
// ---------------------------------------------------------------------------

// EffectiveMode resolves the configured mode, consulting the capability probe
// for "auto". It returns one of config.XFFOff, config.XFFIPv4, config.XFFIPv6,
// config.XFFMixed.
func EffectiveMode(s config.Settings, probe *model.XFFProbeRecord) string {
	switch s.XFFMode {
	case config.XFFIPv6:
		// Forging IPv6 without proven support would burn the whole submit
		// deadline, so degrade to IPv4 unless the probe says otherwise.
		if probe != nil && probe.OK && probe.IPv6Accepted {
			return config.XFFIPv6
		}
		if probe != nil && probe.OK && probe.IPv4Accepted {
			return config.XFFIPv4
		}
		return config.XFFIPv6
	case config.XFFAuto:
		if probe != nil && probe.OK {
			switch {
			case probe.IPv6Accepted && probe.IPv4Accepted:
				return config.XFFMixed
			case probe.IPv6Accepted:
				return config.XFFIPv6
			case probe.IPv4Accepted:
				return config.XFFIPv4
			}
		}
		return config.XFFIPv4
	case config.XFFMixed:
		return config.XFFMixed
	case config.XFFOff:
		return config.XFFOff
	default:
		return config.XFFIPv4
	}
}

// PickFamily chooses the address family for the next identity given the
// effective mode.
func PickFamily(effective string) string {
	switch effective {
	case config.XFFOff:
		return model.FamilyNone
	case config.XFFIPv6:
		return model.FamilyIPv6
	case config.XFFMixed:
		if randBit() {
			return model.FamilyIPv6
		}
		return model.FamilyIPv4
	default:
		return model.FamilyIPv4
	}
}
