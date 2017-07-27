// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nat

import (
	"github.com/intel-go/yanff/common"
	"github.com/intel-go/yanff/flow"
	"github.com/intel-go/yanff/packet"
	"sync"
	"time"
	"unsafe"
)

type Tuple struct {
	addr    uint32
	port    uint16
}

type TupleKey struct {
	Tuple
	protocol uint8
}

var (
	PublicMAC, PrivateMAC [common.EtherAddrLen]uint8
	Natconfig             *Config
	// Main lookup table which contains entries
	table                 map[TupleKey]Tuple
	mutex                 sync.Mutex

	EMPTY_ENTRY = Tuple{ addr: 0, port: 0, }
)

func init() {
	table = make(map[TupleKey]Tuple)
}

func allocateNewEgressConnection(protocol uint8, privEntry TupleKey, publicAddr uint32) {
	pubEntry := TupleKey{
		Tuple: Tuple{
			addr: publicAddr,
			port: uint16(allocNewPort(protocol)),
		},
		protocol: privEntry.protocol,
	}

	table[privEntry] = pubEntry.Tuple
	table[pubEntry] = privEntry.Tuple
	portmap[privEntry.protocol][pubEntry.port].lastused = time.Now()
}

// Ingress translation
func PublicToPrivateTranslation(pkt *packet.Packet, ctx flow.UserContext) bool {
	l3offset := pkt.ParseL2()
	var l4offset int

	// Parse packet type and address
	if pkt.Ether.EtherType == packet.SwapBytesUint16(common.IPV4Number) {
		pkt.IPv4 = (*packet.IPv4Hdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l3offset)))
		l4offset = l3offset + int((pkt.IPv4.VersionIhl & 0x0f) << 2)
	} else {
		// We don't currently support anything except for IPv4
		return false
	}

	// Create a lookup key
	protocol := pkt.IPv4.NextProtoID
	pub2priKey := TupleKey{
		Tuple: Tuple{
			addr: pkt.IPv4.DstAddr,
		},
		protocol: protocol,
	}
	// Parse packet destination port
	if protocol == common.TCPNumber {
		pkt.TCP = (*packet.TCPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pub2priKey.Tuple.port = pkt.TCP.DstPort
	} else if protocol == common.UDPNumber {
		pkt.UDP = (*packet.UDPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pub2priKey.Tuple.port = pkt.UDP.DstPort
	} else if protocol == common.ICMPNumber {
		pkt.ICMP = (*packet.ICMPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pub2priKey.Tuple.port = pkt.ICMP.Identifier
	} else {
		return false
	}

	// Do lookup
	mutex.Lock()
	value := table[pub2priKey]
	// For ingress connections packets are allowed only if a
	// connection has been previosly established with a egress
	// (private to public) packet. So if lookup fails, this incoming
	// packet is ignored.
	if value == EMPTY_ENTRY {
		mutex.Unlock()
		return false
	} else {
		// Check whether connection is too old
		if portmap[protocol][pub2priKey.port].lastused.Add(CONNECTION_TIMEOUT).After(time.Now()) {
			portmap[protocol][pub2priKey.port].lastused = time.Now()
		} else {
			// There was no transfer on this port for too long
			// time. We don't allow it any more
			deleteOldConnection(protocol, int(pub2priKey.port))
		}
	}
	mutex.Unlock()

	// Do packet translation
	pkt.Ether.DAddr = Natconfig.PrivatePort.DstMACAddress
	pkt.Ether.SAddr = PrivateMAC
	pkt.IPv4.DstAddr = value.addr

	if pkt.IPv4.NextProtoID == common.TCPNumber {
		pkt.TCP.DstPort = value.port
	} else if pkt.IPv4.NextProtoID == common.UDPNumber {
		pkt.UDP.DstPort = value.port
	} else {
		// Only address is not modified in ICMP packets
	}

	return true
}

// Egress translation
func PrivateToPublicTranslation(pkt *packet.Packet, ctx flow.UserContext) bool {
	l3offset := pkt.ParseL2()
	var l4offset int

	// Parse packet type and address
	if pkt.Ether.EtherType == packet.SwapBytesUint16(common.IPV4Number) {
		pkt.IPv4 = (*packet.IPv4Hdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l3offset)))
		l4offset = l3offset + int((pkt.IPv4.VersionIhl & 0x0f) << 2)
	} else {
		// We don't currently support anything except for IPv4
		return false
	}

	// Create a lookup key
	protocol := pkt.IPv4.NextProtoID
	pri2pubKey := TupleKey{
		Tuple: Tuple{
			addr: pkt.IPv4.SrcAddr,
		},
		protocol: protocol,
	}

	// Parse packet source port
	if protocol == common.TCPNumber {
		pkt.TCP = (*packet.TCPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pri2pubKey.Tuple.port = pkt.TCP.SrcPort
	} else if protocol == common.UDPNumber {
		pkt.UDP = (*packet.UDPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pri2pubKey.Tuple.port = pkt.UDP.SrcPort
	} else if protocol == common.ICMPNumber {
		pkt.ICMP = (*packet.ICMPHdr)(unsafe.Pointer(pkt.Unparsed + uintptr(l4offset)))
		pri2pubKey.Tuple.port = pkt.ICMP.Identifier
	} else {
		return false
	}

	// Do lookup
	mutex.Lock()
	value := table[pri2pubKey]
	if value == EMPTY_ENTRY {
		allocateNewEgressConnection(protocol, pri2pubKey, Natconfig.PublicPort.Address)
	} else {
		portmap[protocol][value.port].lastused = time.Now()
	}
	mutex.Unlock()

	// Do packet translation
	pkt.Ether.DAddr = Natconfig.PublicPort.DstMACAddress
	pkt.Ether.SAddr = PublicMAC
	pkt.IPv4.SrcAddr = value.addr

	if pkt.IPv4.NextProtoID == common.TCPNumber {
		pkt.TCP.SrcPort = value.port
	} else if pkt.IPv4.NextProtoID == common.UDPNumber {
		pkt.UDP.SrcPort = value.port
	} else {
		// Only address is not modified in ICMP packets
	}

	return true
}
