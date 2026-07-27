package smb2

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

// TestNetworkInterfaceInfoLayout checks a single interface against the layout of the
// NETWORK_INTERFACE_INFO structure, field by field.
func TestNetworkInterfaceInfoLayout(t *testing.T) {
	tests := []struct {
		name  string
		iface NetworkInterface

		// The part of the SockAddr_Storage field that carries the address. Everything
		// beyond it is padding and has to be zero.
		wantSockAddr []byte
	}{
		{
			name:  "IPv4",
			iface: NetworkInterface{Index: 7, Capability: NIC_RSS_CAPABLE, LinkSpeed: 1_000_000_000, Address: netip.MustParseAddr("192.168.1.50")},
			wantSockAddr: []byte{
				0x02, 0x00, // Family
				0x00, 0x00, // Port
				192, 168, 1, 50, // IPv4Address, in network byte order
				0, 0, 0, 0, 0, 0, 0, 0, // Reserved
			},
		},
		{
			name:  "IPv6",
			iface: NetworkInterface{Index: 3, Capability: NIC_RDMA_CAPABLE, LinkSpeed: 10_000_000_000, Address: netip.MustParseAddr("2001:db8::1")},
			wantSockAddr: []byte{
				0x17, 0x00, // Family
				0x00, 0x00, // Port
				0x00, 0x00, 0x00, 0x00, // FlowInfo
				0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01, // IPv6Address
				0x00, 0x00, 0x00, 0x00, // ScopeId
			},
		},
		{
			// An IPv4 address in its mapped form has to go out as an IPv4 one, or the
			// client would try to reach the server over IPv6.
			name:  "IPv4 mapped into IPv6",
			iface: NetworkInterface{Index: 1, Address: netip.MustParseAddr("::ffff:10.0.0.1")},
			wantSockAddr: []byte{
				0x02, 0x00,
				0x00, 0x00,
				10, 0, 0, 1,
				0, 0, 0, 0, 0, 0, 0, 0,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := NetworkInterfaceInfo([]NetworkInterface{test.iface})
			if len(buf) != NetworkInterfaceInfoSize {
				t.Fatalf("len = %d, want %d", len(buf), NetworkInterfaceInfoSize)
			}

			// A single interface is the last one in the chain, so it points at nothing.
			if next := binary.LittleEndian.Uint32(buf[0:4]); next != 0 {
				t.Errorf("Next = %d, want 0", next)
			}
			if index := binary.LittleEndian.Uint32(buf[4:8]); index != test.iface.Index {
				t.Errorf("IfIndex = %d, want %d", index, test.iface.Index)
			}
			if capability := binary.LittleEndian.Uint32(buf[8:12]); capability != test.iface.Capability {
				t.Errorf("Capability = %#x, want %#x", capability, test.iface.Capability)
			}
			if reserved := binary.LittleEndian.Uint32(buf[12:16]); reserved != 0 {
				t.Errorf("Reserved = %d, want 0", reserved)
			}
			if speed := binary.LittleEndian.Uint64(buf[16:24]); speed != test.iface.LinkSpeed {
				t.Errorf("LinkSpeed = %d, want %d", speed, test.iface.LinkSpeed)
			}

			sockAddr := buf[24:]
			if !bytes.Equal(sockAddr[:len(test.wantSockAddr)], test.wantSockAddr) {
				t.Errorf("SockAddr_Storage = % x, want % x", sockAddr[:len(test.wantSockAddr)], test.wantSockAddr)
			}
			for i, b := range sockAddr[len(test.wantSockAddr):] {
				if b != 0 {
					t.Errorf("padding of SockAddr_Storage is not zero at %d: %#x", len(test.wantSockAddr)+i, b)
					break
				}
			}
		})
	}
}

// TestNetworkInterfaceInfoChain checks that several interfaces are chained together, since
// the client walks the list by the offsets rather than by a count.
func TestNetworkInterfaceInfoChain(t *testing.T) {
	ifaces := []NetworkInterface{
		{Index: 2, Address: netip.MustParseAddr("10.0.0.1")},
		{Index: 3, Address: netip.MustParseAddr("2001:db8::1")},
		{Index: 4, Address: netip.MustParseAddr("10.0.0.2")},
	}

	buf := NetworkInterfaceInfo(ifaces)
	if len(buf) != NetworkInterfaceInfoSize*len(ifaces) {
		t.Fatalf("len = %d, want %d", len(buf), NetworkInterfaceInfoSize*len(ifaces))
	}

	// Walk the chain the way the client does, rather than by indexing into the buffer.
	var off uint32
	for i, iface := range ifaces {
		entry := buf[off:]
		if index := binary.LittleEndian.Uint32(entry[4:8]); index != iface.Index {
			t.Errorf("entry %d: IfIndex = %d, want %d", i, index, iface.Index)
		}

		next := binary.LittleEndian.Uint32(entry[0:4])
		want := uint32(NetworkInterfaceInfoSize)
		if i == len(ifaces)-1 { // The last entry ends the chain
			want = 0
		}
		if next != want {
			t.Fatalf("entry %d: Next = %d, want %d", i, next, want)
		}

		off += next
	}

	if off != uint32(len(buf))-NetworkInterfaceInfoSize {
		t.Errorf("the chain ended at %d, want %d", off, len(buf)-NetworkInterfaceInfoSize)
	}
}

func TestNetworkInterfaceInfoEmpty(t *testing.T) {
	if buf := NetworkInterfaceInfo(nil); len(buf) != 0 {
		t.Errorf("len = %d, want 0", len(buf))
	}
}
