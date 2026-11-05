package waypoint

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestParseAttachmentAddressAcceptsHostAddresses(t *testing.T) {
	for cidr, want := range map[string]int{
		"10.89.0.2/24":   24,
		"10.89.0.254/24": 24,
		"192.168.7.9/16": 16,
		"10.0.0.1/8":     8,
		"10.89.0.1/30":   30,
	} {
		ip, n, err := parseAttachmentAddress(cidr)
		if err != nil {
			t.Fatalf("parseAttachmentAddress(%q) = %v", cidr, err)
		}
		if ones, _ := n.Mask.Size(); ones != want || len(ip) != 4 {
			t.Fatalf("parseAttachmentAddress(%q) = %v %v", cidr, ip, n)
		}
	}
}

func TestParseAttachmentAddressRejectsOthers(t *testing.T) {
	for cidr, want := range map[string]string{
		"10.89.0.2":      "want IPv4 CIDR",
		"fd00::2/64":     "only IPv4",
		"10.89.0.2/31":   "prefix",
		"10.89.0.2/7":    "prefix",
		"10.89.0.0/24":   "not a host address",
		"10.89.0.255/24": "not a host address",
		"169.254.1.2/16": "not a host address",
		"224.0.0.5/24":   "not a host address",
		"127.0.0.2/8":    "not a host address",
	} {
		if _, _, err := parseAttachmentAddress(cidr); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("parseAttachmentAddress(%q) = %v, want error containing %q", cidr, err, want)
		}
	}
}

func TestOverlappingNetwork(t *testing.T) {
	mustNet := func(s string) *net.IPNet {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	host := []net.Addr{mustNet("127.0.0.1/8"), mustNet("10.0.2.15/24"), mustNet("fe80::1/64")}
	_, fabric, _ := net.ParseCIDR("10.89.0.0/24")
	if got := overlappingNetwork(fabric, host); got != "" {
		t.Fatalf("disjoint subnet reported overlap with %s", got)
	}
	for _, s := range []string{"10.0.2.0/24", "10.0.0.0/16", "10.0.2.128/25"} {
		_, n, _ := net.ParseCIDR(s)
		if got := overlappingNetwork(n, host); got != "10.0.2.15/24" {
			t.Fatalf("overlappingNetwork(%s) = %q, want 10.0.2.15/24", s, got)
		}
	}
	_, lo, _ := net.ParseCIDR("127.0.0.0/16")
	if got := overlappingNetwork(lo, host); got != "" {
		t.Fatalf("loopback counted as a host network: %s", got)
	}
}

func TestFabricPeerNameIsStableAndFitsIFNAMSIZ(t *testing.T) {
	a := fabricPeerName("a1b2c3d4e5f6a7b8", "r1")
	if a != fabricPeerName("a1b2c3d4e5f6a7b8", "r1") {
		t.Fatal("peer name is not deterministic")
	}
	if len(a) >= 16 || !strings.HasPrefix(a, "wp") {
		t.Fatalf("peer name %q is not a short wp-prefixed interface name", a)
	}
	for _, other := range []string{fabricPeerName("a1b2c3d4e5f6a7b8", "r2"), fabricPeerName("b1b2c3d4e5f6a7b8", "r1")} {
		if other == a {
			t.Fatalf("distinct forks share peer name %q", a)
		}
	}
}

func TestRtattrPadsToFourBytes(t *testing.T) {
	for data, size := range map[string]int{"": 4, "a": 8, "abcd": 8, "abcde": 12} {
		b := rtattr(7, []byte(data))
		if len(b) != size {
			t.Fatalf("rtattr(%q) is %d bytes, want %d", data, len(b), size)
		}
		if l := binary.NativeEndian.Uint16(b); int(l) != 4+len(data) {
			t.Fatalf("rtattr(%q) length field %d, want %d", data, l, 4+len(data))
		}
		if k := binary.NativeEndian.Uint16(b[2:]); k != 7 {
			t.Fatalf("rtattr(%q) type %d", data, k)
		}
	}
}

func TestForkRecordKeepsAttachment(t *testing.T) {
	f := Fork{ID: "r1", ForkNetwork: ForkNetwork{
		NetNS:      "/run/waypoint/s/r1/netns",
		Attachment: &NetworkAttachment{FabricNS: "/run/netns/fab-r1", Address: "10.89.0.2/24"},
		FabricPeer: "wp01234567",
	}}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var got Fork
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Attachment == nil || *got.Attachment != *f.Attachment || got.FabricPeer != f.FabricPeer {
		t.Fatalf("round trip lost the attachment: %s", data)
	}
	got.releaseNetwork() // no pasta or pin to release; clears the record
	if got.Attachment != nil || got.FabricPeer != "" {
		t.Fatal("releaseNetwork kept the attachment")
	}
}

func TestSharedSessionRefusesAttachment(t *testing.T) {
	m := &Manager{sessionID: "s", network: NetworkShared}
	err := m.AttachMain(NetworkAttachment{FabricNS: "/run/netns/x", Address: "10.89.0.2/24"})
	if err == nil || !strings.Contains(err.Error(), NetworkPrivate) {
		t.Fatalf("AttachMain on a shared session = %v", err)
	}
	if m.mainAttach != nil {
		t.Fatal("refused attachment was kept")
	}
}

func TestCheckAttachmentRejectsNonNamespace(t *testing.T) {
	err := checkAttachment(NetworkAttachment{FabricNS: t.TempDir(), Address: "10.89.0.2/24"})
	if err == nil || !strings.Contains(err.Error(), "not a network namespace") {
		t.Fatalf("checkAttachment on a directory = %v", err)
	}
}
