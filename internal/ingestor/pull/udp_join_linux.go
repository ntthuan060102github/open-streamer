//go:build linux

package pull

// udp_join_linux.go — Linux multicast join via raw setsockopt(IP_ADD_MEMBERSHIP).
//
// We bypass golang.org/x/net/ipv4.PacketConn.JoinGroup here because that path
// internally enumerates AF_INET6 routes through netlink to resolve the
// interface — and it fails with
//
//	"netlinkrib: address family not supported by protocol"
//
// on hosts where IPv6 is disabled via sysctl
// (`net.ipv6.conf.all.disable_ipv6=1`), which is common in production
// deployments behind IPv4-only multicast feeds. The kernel still lets us
// issue IP_ADD_MEMBERSHIP with the interface index directly, so we do exactly
// that and skip the netlink lookup entirely.
//
// IPMreqn (Linux extension of POSIX IPMreq) carries the ifindex, so the
// kernel doesn't need to resolve the interface's IP address either.

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// joinIPv4ASMWithInterface joins the IPv4 ASM (any-source multicast) group
// `group` on `iface` using setsockopt(IP_ADD_MEMBERSHIP) with IPMreqn.
func joinIPv4ASMWithInterface(conn *net.UDPConn, group net.IP, iface *net.Interface) error {
	g4 := group.To4()
	if g4 == nil {
		return fmt.Errorf("udp: group %s is not IPv4", group)
	}
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("udp: syscall conn: %w", err)
	}
	mreq := &unix.IPMreqn{
		Multiaddr: [4]byte{g4[0], g4[1], g4[2], g4[3]},
		Ifindex:   int32(iface.Index),
	}
	var sockErr error
	cerr := rawConn.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptIPMreqn(int(fd), unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, mreq)
	})
	if cerr != nil {
		return fmt.Errorf("udp: control fd: %w", cerr)
	}
	if sockErr != nil {
		return fmt.Errorf("udp: IP_ADD_MEMBERSHIP iface=%s ifindex=%d: %w",
			iface.Name, iface.Index, sockErr)
	}
	return nil
}
