// Package snmp implements a minimal SNMPv1 GET client for polling the Samsung
// M2070W printer state machine.
//
// The printer exposes a per-registration state OID:
//
//	1.3.6.1.4.1.236.11.5.11.81.11.7.2.1.2.{InstanceID}
//
// where InstanceID is returned by the S2PC_Regi ADD HTTP call. Querying this
// OID over UDP port 161 returns one of three states: Idle (0x00), Triggered
// (0x01, user opened the scan menu), or Ready (0x02, user confirmed the scan).
//
// The printer returns the value as an OCTET STRING (ASN.1 tag 0x04) rather than
// an INTEGER; both tags are accepted. Both big-endian and little-endian 4-byte
// encodings are observed in practice and both are handled.
//
// Poll distinguishes "the printer answered for our OID" from "it didn't": on a
// read timeout, or when the response carries no value for our OID (the printer
// forgot our registration / noSuchName), Poll returns ErrNoResponse. Callers use
// this to detect that the printer was power-cycled and re-register.
package snmp

import (
	"errors"
	"net"
	"time"
)

// ErrNoResponse means the printer did not return a usable value for our OID —
// either it never answered (timeout) or it answered without our registration's
// value (e.g. after a power-cycle dropped the Scan2PC table). It is distinct from
// a genuine Idle state, which arrives as (Idle, nil).
var ErrNoResponse = errors.New("snmp: no response for OID")

// OID: 1.3.6.1.4.1.236.11.5.11.81.11.7.2.1.2.{InstanceID}
var oidBase = []int{1, 3, 6, 1, 4, 1, 236, 11, 5, 11, 81, 11, 7, 2, 1, 2}

const (
	community = "public"
	port      = "161"
)

// State represents the printer scan state machine.
type State int

const (
	Idle      State = 0
	Triggered State = 1
	Ready     State = 2
)

func (s State) String() string {
	switch s {
	case Triggered:
		return "triggered"
	case Ready:
		return "ready"
	default:
		return "idle"
	}
}

// encodeOID encodes a sequence of OID arcs to BER bytes (tag 0x06 included).
func encodeOID(arcs []int) []byte {
	// First two arcs are combined per BER: 40*arc[0] + arc[1]
	body := []byte{byte(40*arcs[0] + arcs[1])}
	for _, arc := range arcs[2:] {
		if arc < 0x80 {
			body = append(body, byte(arc))
		} else {
			// base-128 big-endian, high bit set on all bytes except the last
			parts := []byte{}
			for arc > 0 {
				parts = append([]byte{byte(arc & 0x7f)}, parts...)
				arc >>= 7
			}
			for i, p := range parts {
				if i < len(parts)-1 {
					body = append(body, p|0x80)
				} else {
					body = append(body, p)
				}
			}
		}
	}
	return append([]byte{0x06, byte(len(body))}, body...)
}

func tlv(tag byte, value []byte) []byte {
	return append([]byte{tag, byte(len(value))}, value...)
}

// buildGetRequest constructs a minimal SNMPv1 GET-Request PDU.
func buildGetRequest(instanceID int) []byte {
	arcs := make([]int, len(oidBase)+1)
	copy(arcs, oidBase)
	arcs[len(oidBase)] = instanceID

	oid := encodeOID(arcs)
	null := []byte{0x05, 0x00}
	varbind := tlv(0x30, concat(oid, null))
	varbindList := tlv(0x30, varbind)

	pduBody := concat(
		tlv(0x02, []byte{0x01}), // request-id = 1
		tlv(0x02, []byte{0x00}), // error-status = 0
		tlv(0x02, []byte{0x00}), // error-index = 0
		varbindList,
	)
	pdu := tlv(0xa0, pduBody)

	msgBody := concat(
		tlv(0x02, []byte{0x00}), // version SNMPv1
		tlv(0x04, []byte(community)),
		pdu,
	)
	return tlv(0x30, msgBody)
}

func concat(slices ...[]byte) []byte {
	var out []byte
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}

// parseResponse extracts the 4-byte value from an SNMPv1 GET-Response.
// The Samsung M2070W returns the state as OCTET STRING (0x04); INTEGER (0x02) is
// also accepted for compatibility.
//
// The message is walked element by element down to the varbind value. Scanning
// for a tag/length byte pair is not safe: the OID ends in arcs "2.{InstanceID}",
// so InstanceID 2 or 4 produces bytes that look like a 4-byte INTEGER header.
func parseResponse(data []byte) []byte {
	_, msg, _ := nth(data, 0)  // Message SEQUENCE
	_, pdu, _ := nth(msg, 2)   // skip version, community → GetResponse-PDU
	_, vbl, _ := nth(pdu, 3)   // skip request-id, error-status, error-index → varbind list
	_, vb, _ := nth(vbl, 0)    // first varbind
	tag, val, ok := nth(vb, 1) // skip name (OID) → value
	if !ok || (tag != 0x02 && tag != 0x04) || len(val) != 4 {
		return nil
	}
	return val
}

// nth returns the tag and value of the n-th (0-based) BER element in b.
// ok is false if b holds fewer than n+1 complete elements.
func nth(b []byte, n int) (tag byte, value []byte, ok bool) {
	for i := 0; ; i++ {
		tag, value, b, ok = readTLV(b)
		if !ok || i == n {
			return tag, value, ok
		}
	}
}

// readTLV splits the first BER element off b, returning its tag, its value and
// the bytes that follow. ok is false if b is truncated.
func readTLV(b []byte) (tag byte, value, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	n, hdr := int(b[1]), 2
	if n&0x80 != 0 { // long form: low 7 bits give the number of length bytes
		k := n & 0x7f
		if k == 0 || k > 2 || len(b) < 2+k {
			return 0, nil, nil, false
		}
		n = 0
		for _, lb := range b[2 : 2+k] {
			n = n<<8 | int(lb)
		}
		hdr += k
	}
	if len(b) < hdr+n {
		return 0, nil, nil, false
	}
	return b[0], b[hdr : hdr+n], b[hdr+n:], true
}

// Poll queries the printer's scan-state OID and returns the current State.
// A parsed value returns (state, nil). A dial/write failure returns the raw
// network error. A read timeout, or a response that carries no value for our OID
// (the printer forgot our registration), returns (Idle, ErrNoResponse).
func Poll(ip string, instanceID int, timeout time.Duration) (State, error) {
	return pollAddr(net.JoinHostPort(ip, port), instanceID, timeout)
}

// pollAddr is Poll against an explicit host:port. Poll always targets the
// printer's privileged port 161, which a test cannot bind, so tests call this
// directly to reach a mock server on an ephemeral port — exercising the real
// request/parse logic rather than a copy of it.
func pollAddr(addr string, instanceID int, timeout time.Duration) (State, error) {
	req := buildGetRequest(instanceID)

	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return Idle, err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
	if _, err := conn.Write(req); err != nil {
		return Idle, err
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		// No reply (printer offline or unreachable) — signal it so the caller
		// can decide whether to re-register.
		return Idle, ErrNoResponse
	}

	val := parseResponse(buf[:n])
	if val == nil {
		// Printer answered but our OID has no value (e.g. noSuchName after a
		// power-cycle dropped the registration).
		return Idle, ErrNoResponse
	}
	return parseValue(val), nil
}

// parseValue maps a 4-byte OID value (big- or little-endian) to a State.
func parseValue(val []byte) State {
	switch {
	case bytesEqual(val, []byte{0x00, 0x00, 0x00, 0x01}),
		bytesEqual(val, []byte{0x01, 0x00, 0x00, 0x00}):
		return Triggered
	case bytesEqual(val, []byte{0x00, 0x00, 0x00, 0x02}),
		bytesEqual(val, []byte{0x02, 0x00, 0x00, 0x00}):
		return Ready
	}
	return Idle
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
