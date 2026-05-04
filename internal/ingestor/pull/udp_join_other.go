//go:build !linux

package pull

// udp_join_other.go — non-Linux fallback for IPv4 ASM multicast join with
// explicit interface. Uses net.InterfaceByName + golang.org/x/net/ipv4 which
// works fine on Darwin / BSD; the netlink-IPv6 issue we work around on Linux
// is Linux-specific.

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

func joinIPv4ASMByIfaceName(conn *net.UDPConn, group net.IP, ifaceName string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface %q: %w", ifaceName, err)
	}
	return ipv4.NewPacketConn(conn).JoinGroup(iface, &net.UDPAddr{IP: group})
}
