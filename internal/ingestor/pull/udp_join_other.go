//go:build !linux

package pull

// udp_join_other.go — non-Linux fallback for multicast join with explicit
// interface. Uses golang.org/x/net/ipv4 which works fine on Darwin / BSD;
// the netlink-IPv6 issue we work around on Linux is Linux-specific.

import (
	"net"

	"golang.org/x/net/ipv4"
)

func joinIPv4ASMWithInterface(conn *net.UDPConn, group net.IP, iface *net.Interface) error {
	return ipv4.NewPacketConn(conn).JoinGroup(iface, &net.UDPAddr{IP: group})
}
