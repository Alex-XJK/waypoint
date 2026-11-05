package waypoint

// Fabric attachment: in private network mode a fork's namespace can also be
// connected, by a veth pair, to a bridge in a caller-owned "fabric" network
// namespace. Forks of different sessions attached to one fabric reach each
// other directly, without pasta, so TCP connections between them are part of
// each fork's checkpointed state; giving every copy of a fork the same address
// on its own fabric lets those connections restore into any number of copies.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// FabricBridge is the bridge a fabric namespace must hold.
	FabricBridge = "br0"
	// fabricIfName is the fork-side end of the veth pair.
	fabricIfName = "wp0"

	vethInfoPeer = 1 // VETH_INFO_PEER, not exported by x/sys/unix
)

// NetworkAttachment connects a fork to a fabric. Address is the fork's IPv4
// address in CIDR form; the fabric's owner assigns addresses and owns the
// fabric's lifecycle, including keeping addresses unique on it.
type NetworkAttachment struct {
	FabricNS string `json:"fabric_netns"`
	Address  string `json:"address"`
}

// fabricPeerName names the fabric-side end of a fork's veth pair. It is stable
// per session and fork, so a leftover end from an earlier fork with the same
// ID can be recognized and replaced.
func fabricPeerName(sessionID, forkID string) string {
	sum := sha256.Sum256([]byte(sessionID + "/" + forkID))
	return "wp" + hex.EncodeToString(sum[:4])
}

// parseAttachmentAddress returns the address and its network; only IPv4
// unicast host addresses on prefixes of /8 to /30 are accepted.
func parseAttachmentAddress(cidr string) (net.IP, *net.IPNet, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid fabric address %q: want IPv4 CIDR such as 10.89.0.2/24", cidr)
	}
	ip4 := ip.To4()
	ones, bits := ipNet.Mask.Size()
	if ip4 == nil || bits != 32 {
		return nil, nil, fmt.Errorf("invalid fabric address %q: only IPv4 is supported", cidr)
	}
	if ones < 8 || ones > 30 {
		return nil, nil, fmt.Errorf("invalid fabric address %q: prefix must be /8 to /30", cidr)
	}
	broadcast := make(net.IP, 4)
	for i := range broadcast {
		broadcast[i] = ipNet.IP[i] | ^ipNet.Mask[i]
	}
	if ip4.Equal(ipNet.IP) || ip4.Equal(broadcast) || !ip4.IsGlobalUnicast() {
		return nil, nil, fmt.Errorf("invalid fabric address %q: not a host address on its subnet", cidr)
	}
	return ip4, ipNet, nil
}

// overlappingNetwork returns the first of addrs whose network overlaps subnet.
// pasta copies the host's addresses and routes into every fork, so a fabric
// subnet overlapping them would make routing in the fork ambiguous.
func overlappingNetwork(subnet *net.IPNet, addrs []net.Addr) string {
	for _, addr := range addrs {
		n, ok := addr.(*net.IPNet)
		if !ok || n.IP.To4() == nil || n.IP.IsLoopback() {
			continue
		}
		if n.Contains(subnet.IP) || subnet.Contains(n.IP.Mask(n.Mask)) {
			return n.String()
		}
	}
	return ""
}

// CheckNetworkAttachment validates a against the configuration and the host
// without changing anything: new sessions must use the private network mode,
// the address must be valid and clear of the host's networks, and the fabric
// must be a network namespace holding FabricBridge.
func CheckNetworkAttachment(a NetworkAttachment) error {
	loadConfig()
	if NetworkMode != NetworkPrivate {
		return fmt.Errorf("fabric attachment needs the %s network mode (set WAYPOINT_NETWORK=%s)", NetworkPrivate, NetworkPrivate)
	}
	return checkAttachment(a)
}

func checkAttachment(a NetworkAttachment) error {
	_, subnet, err := parseAttachmentAddress(a.Address)
	if err != nil {
		return err
	}
	if hostAddrs, err := net.InterfaceAddrs(); err == nil {
		if n := overlappingNetwork(subnet, hostAddrs); n != "" {
			return fmt.Errorf("fabric subnet %s overlaps host network %s, which pasta copies into every fork", subnet, n)
		}
	}
	fabric, err := openNetNS(a.FabricNS)
	if err != nil {
		return err
	}
	defer fabric.Close()
	return withThreadNetNS(enterNetNS(fabric), func() error {
		if _, err := net.InterfaceByName(FabricBridge); err != nil {
			return fmt.Errorf("fabric %s has no bridge %s: %w", a.FabricNS, FabricBridge, err)
		}
		return nil
	})
}

// openNetNS opens path and verifies that it is a network namespace.
func openNetNS(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open fabric network namespace: %w", err)
	}
	if kind, err := unix.IoctlRetInt(int(f.Fd()), unix.NS_GET_NSTYPE); err != nil || kind != unix.CLONE_NEWNET {
		f.Close()
		return nil, fmt.Errorf("%s is not a network namespace", path)
	}
	return f, nil
}

func enterNetNS(ns *os.File) func() error {
	return func() error {
		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("enter network namespace %s: %w", ns.Name(), err)
		}
		return nil
	}
}

// attachFabric creates the veth pair from the fork namespace at pin to the
// fabric: wp0 with a's address in the fork, the peer on the fabric's bridge.
// Both ends are up when it returns. A leftover peer of the same name belongs
// to an earlier fork with this ID whose namespace is still being torn down,
// and is replaced.
func attachFabric(pin string, a NetworkAttachment, peer string) error {
	ip, ipNet, err := parseAttachmentAddress(a.Address)
	if err != nil {
		return err
	}
	prefix, _ := ipNet.Mask.Size()
	fabric, err := openNetNS(a.FabricNS)
	if err != nil {
		return err
	}
	defer fabric.Close()
	fork, err := os.Open(pin)
	if err != nil {
		return fmt.Errorf("open fork network namespace: %w", err)
	}
	defer fork.Close()

	err = withThreadNetNS(enterNetNS(fabric), func() error {
		if _, err := net.InterfaceByName(FabricBridge); err != nil {
			return fmt.Errorf("fabric %s has no bridge %s: %w", a.FabricNS, FabricBridge, err)
		}
		return deleteLink(peer)
	})
	if err != nil {
		return err
	}

	err = withThreadNetNS(enterNetNS(fork), func() error {
		peerInfo := append(linkMsg(0, 0), rtattr(unix.IFLA_IFNAME, cString(peer))...)
		peerInfo = append(peerInfo, rtattr(unix.IFLA_NET_NS_FD, u32(uint32(fabric.Fd())))...)
		linkInfo := append(rtattr(unix.IFLA_INFO_KIND, cString("veth")),
			rtattr(unix.IFLA_INFO_DATA, rtattr(vethInfoPeer, peerInfo))...)
		attrs := append(rtattr(unix.IFLA_IFNAME, cString(fabricIfName)), rtattr(unix.IFLA_LINKINFO, linkInfo)...)
		if err := rtnl(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, linkMsg(0, 0), attrs); err != nil {
			return fmt.Errorf("create veth %s/%s: %w", fabricIfName, peer, err)
		}
		link, err := net.InterfaceByName(fabricIfName)
		if err != nil {
			return err
		}
		addr := make([]byte, unix.SizeofIfAddrmsg)
		addr[0], addr[1] = unix.AF_INET, byte(prefix)
		binary.NativeEndian.PutUint32(addr[4:], uint32(link.Index))
		addrAttrs := append(rtattr(unix.IFA_LOCAL, ip), rtattr(unix.IFA_ADDRESS, ip)...)
		if err := rtnl(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, addr, addrAttrs); err != nil {
			return fmt.Errorf("assign %s to %s: %w", a.Address, fabricIfName, err)
		}
		if err := rtnl(unix.RTM_NEWLINK, 0, linkMsg(link.Index, unix.IFF_UP), nil); err != nil {
			return fmt.Errorf("bring up %s: %w", fabricIfName, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	return withThreadNetNS(enterNetNS(fabric), func() error {
		br, err := net.InterfaceByName(FabricBridge)
		if err != nil {
			return err
		}
		link, err := net.InterfaceByName(peer)
		if err != nil {
			return err
		}
		if err := rtnl(unix.RTM_NEWLINK, 0, linkMsg(link.Index, unix.IFF_UP), rtattr(unix.IFLA_MASTER, u32(uint32(br.Index)))); err != nil {
			return fmt.Errorf("add %s to %s: %w", peer, FabricBridge, err)
		}
		return nil
	})
}

// detachFabric deletes the fabric-side end of a fork's veth pair, and with it
// the fork's end. Otherwise the pair would outlive the call until the kernel
// finishes tearing down the released namespace, leaving the fork's address
// answering on the fabric meanwhile. A fabric that is already gone is fine.
func detachFabric(fabricNS, peer string) error {
	fabric, err := openNetNS(fabricNS)
	if err != nil {
		return nil
	}
	defer fabric.Close()
	return withThreadNetNS(enterNetNS(fabric), func() error { return deleteLink(peer) })
}

// deleteLink deletes the named link of the calling thread's namespace, if any.
func deleteLink(name string) error {
	link, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	if err := rtnl(unix.RTM_DELLINK, 0, linkMsg(link.Index, 0), nil); err != nil && err != unix.ENODEV {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

// --- minimal rtnetlink ---

// linkMsg is an ifinfomsg for link index (0 = new) that sets flags.
func linkMsg(index int, flags uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	b[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:], uint32(index))
	binary.NativeEndian.PutUint32(b[8:], flags)
	binary.NativeEndian.PutUint32(b[12:], flags) // change mask
	return b
}

// rtattr encodes one route attribute, padded to 4 bytes.
func rtattr(kind uint16, data []byte) []byte {
	n := unix.SizeofRtAttr + len(data)
	b := make([]byte, (n+unix.RTA_ALIGNTO-1) & ^(unix.RTA_ALIGNTO-1))
	binary.NativeEndian.PutUint16(b[0:], uint16(n))
	binary.NativeEndian.PutUint16(b[2:], kind)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

func cString(s string) []byte { return append([]byte(s), 0) }

// rtnl sends one request on a NETLINK_ROUTE socket of the calling thread's
// network namespace and waits for its acknowledgement.
func rtnl(msgType uint16, flags int, header, attrs []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}

	msg := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(header)+len(attrs))
	msg = append(append(msg, header...), attrs...)
	const seq = 1
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.NativeEndian.PutUint16(msg[4:], msgType)
	binary.NativeEndian.PutUint16(msg[6:], uint16(unix.NLM_F_REQUEST|unix.NLM_F_ACK|flags))
	binary.NativeEndian.PutUint32(msg[8:], seq)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}

	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq || m.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(m.Data) < 4 {
				return errors.New("short netlink error message")
			}
			if code := int32(binary.NativeEndian.Uint32(m.Data)); code != 0 {
				return unix.Errno(-code)
			}
			return nil
		}
	}
}

// AttachMain makes StartShell connect main to a's fabric before the shell
// starts. The session must use the private network mode.
func (m *Manager) AttachMain(a NetworkAttachment) error {
	if err := m.checkSessionAttachment(a); err != nil {
		return err
	}
	m.mainAttach = &a
	return nil
}

func (m *Manager) checkSessionAttachment(a NetworkAttachment) error {
	if !m.privateNetwork() {
		return fmt.Errorf("session %s uses the %s network mode; fabric attachment needs %s", m.sessionID, NetworkShared, NetworkPrivate)
	}
	return checkAttachment(a)
}
