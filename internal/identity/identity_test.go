package identity

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/model"
)

func TestRandomIPv4IsUsablePublicSpace(t *testing.T) {
	for i := 0; i < 3000; i++ {
		s := RandomIPv4(PoolPublic)
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("generated address %q does not parse: %v", s, err)
		}
		if !addr.Is4() {
			t.Fatalf("expected an IPv4 literal, got %q", s)
		}
		if !addr.IsGlobalUnicast() || addr.IsPrivate() {
			t.Fatalf("generated non-public address %q", s)
		}
		if inAnyPrefix(addr, ipv4Excluded) {
			t.Fatalf("generated excluded address %q", s)
		}
	}
}

func TestRandomIPv4ReservedPoolStaysInDocumentationRanges(t *testing.T) {
	for i := 0; i < 500; i++ {
		addr := netip.MustParseAddr(RandomIPv4(PoolReserved))
		if !inAnyPrefix(addr, ipv4Documentation) {
			t.Fatalf("reserved pool produced %q outside RFC 5737 ranges", addr)
		}
	}
}

func TestRandomIPv6IsGlobalUnicast(t *testing.T) {
	for i := 0; i < 3000; i++ {
		s := RandomIPv6(PoolPublic, false)
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("generated address %q does not parse: %v", s, err)
		}
		if !addr.Is6() || addr.Is4In6() {
			t.Fatalf("expected an IPv6 literal, got %q", s)
		}
		if !addr.IsGlobalUnicast() {
			t.Fatalf("generated non-global-unicast address %q", s)
		}
		if inAnyPrefix(addr, ipv6Excluded) {
			t.Fatalf("generated excluded address %q", s)
		}
		// The generator must stay inside 2000::/3.
		if !netip.MustParsePrefix("2000::/3").Contains(addr) {
			t.Fatalf("generated address %q outside 2000::/3", s)
		}
	}
}

func TestRandomIPv6ReservedPoolStaysInDocumentationRange(t *testing.T) {
	for i := 0; i < 500; i++ {
		addr := netip.MustParseAddr(RandomIPv6(PoolReserved, false))
		if !inAnyPrefix(addr, ipv6Documentation) {
			t.Fatalf("reserved pool produced %q outside 2001:db8::/32", addr)
		}
	}
}

func TestVariantsAreDistinctSpellingsOfTheSameAddress(t *testing.T) {
	const canonical = "2001:4860:4860::8888"
	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		v := Variant(canonical)
		addr, err := netip.ParseAddr(v)
		if err != nil {
			t.Fatalf("variant %q does not parse", v)
		}
		if addr != netip.MustParseAddr(canonical) {
			t.Fatalf("variant %q is a different address", v)
		}
		seen[v]++
	}
	// Upstream counts the raw string, so at least the expanded spelling must
	// appear alongside the compressed one for the option to be meaningful.
	if len(seen) < 2 {
		t.Fatalf("expected multiple spellings, got %v", seen)
	}
	if _, ok := seen[strings.ToUpper(canonical)]; !ok {
		t.Fatalf("expected an upper-case spelling among %v", seen)
	}
}

func TestVariantLeavesIPv4Alone(t *testing.T) {
	const v4 = "198.51.100.7"
	if got := Variant(v4); got != v4 {
		t.Fatalf("Variant(%q) = %q, want unchanged", v4, got)
	}
}

func TestMintAssignsFamilyAndQuota(t *testing.T) {
	ipv6 := Mint(model.FamilyIPv6, PoolPublic, false)
	if ipv6.Family != model.FamilyIPv6 {
		t.Fatalf("family = %q", ipv6.Family)
	}
	if _, err := netip.ParseAddr(ipv6.ForgedIP); err != nil {
		t.Fatalf("forged ip %q does not parse", ipv6.ForgedIP)
	}
	if !strings.HasPrefix(ipv6.ClientID, "mmtrial_") {
		t.Fatalf("client id = %q, want mmtrial_ prefix", ipv6.ClientID)
	}
	if !strings.HasPrefix(ipv6.VisitorID, "mmguest_") {
		t.Fatalf("visitor id = %q, want mmguest_ prefix", ipv6.VisitorID)
	}
	if ipv6.UsesLeft != 2 {
		t.Fatalf("uses left = %d, want 2", ipv6.UsesLeft)
	}

	none := Mint(model.FamilyNone, PoolPublic, false)
	if none.ForgedIP != "" {
		t.Fatalf("family none should not forge an address, got %q", none.ForgedIP)
	}
	if none.UsesLeft < 2 {
		t.Fatalf("family none should not be quota-limited, got %d", none.UsesLeft)
	}
}

func TestMintProducesUniqueIdentities(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := Mint(model.FamilyIPv4, PoolPublic, false)
		if seen[id.ClientID] {
			t.Fatalf("duplicate client id %q", id.ClientID)
		}
		seen[id.ClientID] = true
	}
}

func TestRotatorNeverReusesAnIdentity(t *testing.T) {
	r := NewRotator()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := r.Mint(model.FamilyIPv4, PoolPublic, false)
		if seen[id.ClientID] || seen[id.ForgedIP] {
			t.Fatalf("iteration %d reused an identity: %+v", i, id)
		}
		seen[id.ClientID] = true
		seen[id.ForgedIP] = true
		r.Retire(id)
	}
	st := r.Stats()
	if st.IdentitiesMinted != 20 || st.IdentitiesExhausted != 20 {
		t.Fatalf("stats = %+v, want 20 minted and 20 retired", st)
	}
}

func TestRotatorResetClearsCounters(t *testing.T) {
	r := NewRotator()
	id := r.Mint(model.FamilyIPv6, PoolPublic, false)
	r.Retire(id)
	r.Reset()
	if st := r.Stats(); st.IdentitiesMinted != 0 || st.IdentitiesExhausted != 0 {
		t.Fatalf("stats survived Reset: %+v", st)
	}
}

func TestEffectiveModeAutoUsesProbe(t *testing.T) {
	base := config.DefaultSettings()
	base.XFFMode = config.XFFAuto

	if got := EffectiveMode(base, nil); got != config.XFFIPv4 {
		t.Fatalf("auto without a probe = %q, want ipv4", got)
	}
	probe4 := &model.XFFProbeRecord{OK: true, IPv4Accepted: true}
	if got := EffectiveMode(base, probe4); got != config.XFFIPv4 {
		t.Fatalf("auto with ipv4-only probe = %q, want ipv4", got)
	}
	probe6 := &model.XFFProbeRecord{OK: true, IPv6Accepted: true}
	if got := EffectiveMode(base, probe6); got != config.XFFIPv6 {
		t.Fatalf("auto with ipv6-only probe = %q, want ipv6", got)
	}
	both := &model.XFFProbeRecord{OK: true, IPv4Accepted: true, IPv6Accepted: true}
	if got := EffectiveMode(base, both); got != config.XFFMixed {
		t.Fatalf("auto with both families = %q, want mixed", got)
	}
}

func TestEffectiveModeDegradesUnprovenIPv6(t *testing.T) {
	s := config.DefaultSettings()
	s.XFFMode = config.XFFIPv6
	// A probe that only proved IPv4 must not leave the gateway forging IPv6.
	probe := &model.XFFProbeRecord{OK: true, IPv4Accepted: true}
	if got := EffectiveMode(s, probe); got != config.XFFIPv4 {
		t.Fatalf("unproven IPv6 = %q, want a downgrade to ipv4", got)
	}
}

func TestPickFamily(t *testing.T) {
	cases := map[string]string{
		config.XFFOff:   model.FamilyNone,
		config.XFFIPv4:  model.FamilyIPv4,
		config.XFFIPv6:  model.FamilyIPv6,
		config.XFFMixed: "",
	}
	for mode, want := range cases {
		got := PickFamily(mode)
		if want == "" {
			if got != model.FamilyIPv4 && got != model.FamilyIPv6 {
				t.Fatalf("mixed picked %q", got)
			}
			continue
		}
		if got != want {
			t.Fatalf("PickFamily(%q) = %q, want %q", mode, got, want)
		}
	}
	// Mixed mode must eventually produce both families.
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[PickFamily(config.XFFMixed)] = true
	}
	if !seen[model.FamilyIPv4] || !seen[model.FamilyIPv6] {
		t.Fatalf("mixed mode never produced both families: %v", seen)
	}
}

func TestExpandIPv6(t *testing.T) {
	got := expandIPv6(netip.MustParseAddr("2001:db8::1"))
	want := "2001:0db8:0000:0000:0000:0000:0000:0001"
	if got != want {
		t.Fatalf("expandIPv6 = %q, want %q", got, want)
	}
}
