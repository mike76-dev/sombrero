package smb2

import (
	"encoding/binary"
	"net/netip"
)

const (
	// Network interface capabilities.
	NIC_RSS_CAPABLE  = 0x00000001
	NIC_RDMA_CAPABLE = 0x00000002
)

const (
	// NetworkInterfaceInfoSize is the size of a single NETWORK_INTERFACE_INFO structure,
	// including the SockAddr_Storage field that terminates it.
	NetworkInterfaceInfoSize = 152

	// Address families of the SockAddr_Storage structure.
	addressFamilyIPv4 = 0x0002
	addressFamilyIPv6 = 0x0017
)

// NetworkInterface describes one network interface that the server can be reached at.
type NetworkInterface struct {
	Index      uint32
	Capability uint32
	LinkSpeed  uint64
	Address    netip.Addr
}

// NetworkInterfaceInfo encodes the interfaces as the chain of NETWORK_INTERFACE_INFO
// structures that an FSCTL_QUERY_NETWORK_INTERFACE_INFO request is answered with. The
// client uses the list to decide which addresses to open the further channels of the
// session to.
func NetworkInterfaceInfo(ifaces []NetworkInterface) []byte {
	buf := make([]byte, NetworkInterfaceInfoSize*len(ifaces))
	for i, iface := range ifaces {
		entry := buf[i*NetworkInterfaceInfoSize:][:NetworkInterfaceInfoSize]

		// The offset to the next entry, which the last one in the chain leaves at zero.
		if i < len(ifaces)-1 {
			binary.LittleEndian.PutUint32(entry[:4], NetworkInterfaceInfoSize)
		}
		binary.LittleEndian.PutUint32(entry[4:8], iface.Index)
		binary.LittleEndian.PutUint32(entry[8:12], iface.Capability)
		binary.LittleEndian.PutUint64(entry[16:24], iface.LinkSpeed)

		// The address is written as a sockaddr, whose port is required to be zero: the
		// client connects to the port it is already talking to the server on. Everything
		// the layout doesn't cover stays zero, including the padding that brings the
		// structure up to its fixed size.
		// An IPv4 address handed over in its mapped form is still an IPv4 address, and
		// sending it as one keeps the client from trying to reach it over IPv6.
		address := iface.Address.Unmap()

		sockAddr := entry[24:]
		if address.Is4() {
			//        SOCKADDR_IN
			//   0-2: Family
			//   2-4: Port
			//   4-8: IPv4Address
			//  8-16: Reserved
			binary.LittleEndian.PutUint16(sockAddr[:2], addressFamilyIPv4)
			addr := address.As4()
			copy(sockAddr[4:8], addr[:])
		} else {
			//        SOCKADDR_IN6
			//   0-2: Family
			//   2-4: Port
			//   4-8: FlowInfo
			//  8-24: IPv6Address
			// 24-28: ScopeId
			binary.LittleEndian.PutUint16(sockAddr[:2], addressFamilyIPv6)
			addr := address.As16()
			copy(sockAddr[8:24], addr[:])
		}
	}

	return buf
}
