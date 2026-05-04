//go:build linux

package pull

// udp_join_linux.go — Linux multicast join via raw setsockopt.
//
// We bypass *both* `net.InterfaceByName` and `golang.org/x/net/ipv4.JoinGroup`
// because each one independently goes through netlink to enumerate interfaces
// — and netlink fails with
//
//	"netlinkrib: address family not supported by protocol"
//
// on hosts where IPv6 is disabled via sysctl
// (`net.ipv6.conf.all.disable_ipv6=1`), which is common on production
// boxes carrying IPv4-only multicast feeds.
//
// Resolution path on Linux:
//  1. ioctl(SIOCGIFINDEX) on a stub socket → interface index from name.
//     ioctl is name-keyed, so it doesn't enumerate the table; the
//     IPv6-disabled netlink path is never touched.
//  2. setsockopt(IP_ADD_MEMBERSHIP) with IPMreqn carrying the ifindex.
//     IPMreqn is the Linux extension of POSIX IPMreq that takes an
//     ifindex directly, so the kernel doesn't need to resolve the
//     interface's IP address either.

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// joinIPv4ASMByIfaceName joins the IPv4 ASM (any-source multicast) group on
// the named interface using purely ioctl + setsockopt — no netlink calls.
func joinIPv4ASMByIfaceName(conn *net.UDPConn, group net.IP, ifaceName string) error {
	g4 := group.To4()
	if g4 == nil {
		return fmt.Errorf("udp: group %s is not IPv4", group)
	}
	ifindex, err := ifaceIndexByNameLinux(ifaceName)
	if err != nil {
		return fmt.Errorf("udp: lookup interface %q: %w", ifaceName, err)
	}
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("udp: syscall conn: %w", err)
	}
	mreq := &unix.IPMreqn{
		Multiaddr: [4]byte{g4[0], g4[1], g4[2], g4[3]},
		Ifindex:   int32(ifindex),
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
			ifaceName, ifindex, sockErr)
	}
	return nil
}

// ifaceIndexByNameLinux resolves an interface name to its kernel ifindex via
// SIOCGIFINDEX. Avoids `net.InterfaceByName` whose underlying RTM_GETLINK
// netlink walk fails on IPv6-disabled hosts.
//
// Opens a transient AF_INET dgram socket purely as the ioctl carrier — the
// socket is closed before return. We don't touch the caller's UDP socket so
// the function is safe to call before bind/join setup.
func ifaceIndexByNameLinux(name string) (int, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return 0, fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return 0, fmt.Errorf("new ifreq: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, ifr); err != nil {
		return 0, fmt.Errorf("SIOCGIFINDEX: %w", err)
	}
	idx := int(ifr.Uint32())
	if idx <= 0 {
		return 0, fmt.Errorf("interface %q has no kernel ifindex", name)
	}
	return idx, nil
}
