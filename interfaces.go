package main

import (
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/mike76-dev/sombrero/smb2"
)

// defaultLinkSpeed is reported for an interface whose speed the operating system doesn't
// disclose, which is the usual case for virtual and wireless adapters.
const defaultLinkSpeed = 1_000_000_000

// virtualInterfacePrefixes lists the names of the interfaces that are never offered to a
// client as a channel. They belong to containers, virtual machines and tunnels, which sit on
// networks of their own: a client is generally not on any of them, and an address it cannot
// reach costs it a connection attempt that stalls until it times out.
//
// The bridge that a virtual machine host carries its own address on is commonly called br0,
// so only the br- prefix that Docker gives its user-defined networks is listed, rather than
// br as a whole.
var virtualInterfacePrefixes = []string{
	"br-",       // Docker user-defined bridges
	"cali",      // Calico
	"cni",       // Kubernetes CNI
	"docker",    // Docker default bridge
	"flannel",   // Flannel
	"tailscale", // Tailscale
	"tap",       // Tunnel adapters
	"tun",
	"veth",    // Container ends of the virtual Ethernet pairs
	"vboxnet", // VirtualBox
	"virbr",   // libvirt bridges
	"vmnet",   // VMware
	"vnet",    // libvirt guest adapters
	"wg",      // WireGuard
	"zt",      // ZeroTier
}

// isVirtualInterface reports whether the interface belongs to a container, a virtual machine
// or a tunnel, rather than to a network that the clients of the server are on.
func isVirtualInterface(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

// networkInterfaces lists the interfaces that the server can be reached at. The server
// listens on every address of the machine, so every interface that is up and carries a
// routable address is a possible channel and is offered to the client as one.
func networkInterfaces() []smb2.NetworkInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Println("Couldn't list the network interfaces:", err)
		return nil
	}

	var result []smb2.NetworkInterface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		speed := linkSpeed(iface.Name)
		for _, a := range addrs {
			prefix, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(prefix.IP)
			if !ok {
				continue
			}

			// A link-local address is only reachable from the same segment and needs a
			// scope to be used at all, so offering one merely makes the client attempt a
			// connection that cannot succeed.
			addr = addr.Unmap()
			if !addr.IsGlobalUnicast() || addr.IsLinkLocalUnicast() {
				continue
			}

			// Every interface is announced as capable of receive side scaling, which tells
			// the client that opening more than one connection to it is worth doing. Each
			// connection is served by a thread of its own, so the channels do run in
			// parallel whatever the adapter underneath does with its queues.
			result = append(result, smb2.NetworkInterface{
				Index:      uint32(iface.Index),
				Capability: smb2.NIC_RSS_CAPABLE,
				LinkSpeed:  speed,
				Address:    addr,
			})
		}
	}

	return result
}

// linkSpeed returns the speed of the interface in bits per second. Go doesn't expose it, so
// it is read from sysfs, which reports megabits per second, exists only on Linux, and only
// holds an answer for the adapters whose driver knows one.
func linkSpeed(name string) uint64 {
	buf, err := os.ReadFile("/sys/class/net/" + name + "/speed")
	if err != nil {
		return defaultLinkSpeed
	}

	// An adapter that is up but has no carrier reports a negative speed.
	mbps, err := strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
	if err != nil || mbps <= 0 {
		return defaultLinkSpeed
	}

	return uint64(mbps) * 1_000_000
}
