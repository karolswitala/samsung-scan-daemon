package snmp

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// wrapResponse builds an SNMPv1 GET-Response for the scan-state OID of instanceID,
// carrying value (a complete TLV). Same layout as the Samsung M2070W sends,
// including the community string and the full OID.
func wrapResponse(instanceID int, errStatus, errIndex byte, value []byte) []byte {
	arcs := append(append([]int{}, oidBase...), instanceID)
	varbind := tlv(0x30, concat(encodeOID(arcs), value))
	pdu := tlv(0xa2, concat(
		tlv(0x02, []byte{0x01}),      // request-id
		tlv(0x02, []byte{errStatus}), // error-status
		tlv(0x02, []byte{errIndex}),  // error-index
		tlv(0x30, varbind),
	))
	return tlv(0x30, concat(tlv(0x02, []byte{0x00}), tlv(0x04, []byte(community)), pdu))
}

// wrapOctetString builds a GET-Response wrapping raw4 as OCTET STRING (0x04).
// This matches what the Samsung M2070W actually sends.
func wrapOctetString(raw4 []byte) []byte {
	return wrapResponse(255, 0x00, 0x00, tlv(0x04, raw4))
}

// wrapNoSuchName builds an SNMPv1 GET-Response with error-status=noSuchName(2).
func wrapNoSuchName() []byte {
	return wrapResponse(255, 0x02, 0x01, []byte{0x05, 0x00})
}

// capturedInstance4Idle is a real M2070W response for InstanceID 4 in the idle state.
// The OID's trailing arcs "2.4" encode as 02 04, which a byte-scanning parser
// mistook for a 4-byte INTEGER and read as 04 04 00 00.
var capturedInstance4Idle = []byte{
	0x30, 0x36, 0x02, 0x01, 0x00, 0x04, 0x06, 'p', 'u', 'b', 'l', 'i', 'c', 0xa2, 0x29, 0x02,
	0x04, 0x72, 0x75, 0xc1, 0x0a, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00, 0x30, 0x1b, 0x30, 0x19, 0x06,
	0x11, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x81, 0x6c, 0x0b, 0x05, 0x0b, 0x51, 0x0b, 0x07, 0x02, 0x01,
	0x02, 0x04, 0x04, 0x04, 0x00, 0x00, 0x00, 0x00,
}

// startMockSNMP starts a UDP server that sends resp once then stops.
// Returns the port it's listening on.
func startMockSNMP(t *testing.T, resp []byte) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 1024)
		n, addr, err := conn.ReadFrom(buf)
		if err != nil || n == 0 {
			return
		}
		conn.WriteTo(resp, addr)
	}()
	return port
}

func poll(t *testing.T, resp []byte) State {
	t.Helper()
	p := startMockSNMP(t, resp)
	state, err := pollOnPort("127.0.0.1", 255, time.Second, p)
	if err != nil {
		t.Fatalf("poll error: %v", err)
	}
	return state
}

// pollOnPort is like Poll but with a custom port — used only in tests.
func pollOnPort(ip string, instanceID int, timeout time.Duration, udpPort int) (State, error) {
	req := buildGetRequest(instanceID)
	addr := net.JoinHostPort(ip, itoa(udpPort))
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return Idle, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(req); err != nil {
		return Idle, err
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return Idle, nil
	}
	val := parseResponse(buf[:n])
	switch {
	case bytesEqual(val, []byte{0x00, 0x00, 0x00, 0x01}),
		bytesEqual(val, []byte{0x01, 0x00, 0x00, 0x00}):
		return Triggered, nil
	case bytesEqual(val, []byte{0x00, 0x00, 0x00, 0x02}),
		bytesEqual(val, []byte{0x02, 0x00, 0x00, 0x00}):
		return Ready, nil
	}
	return Idle, nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func TestParseIdle(t *testing.T) {
	state := poll(t, wrapOctetString([]byte{0x00, 0x00, 0x00, 0x00}))
	if state != Idle {
		t.Errorf("want Idle, got %s", state)
	}
}

func TestParseTriggered_BigEndian(t *testing.T) {
	state := poll(t, wrapOctetString([]byte{0x00, 0x00, 0x00, 0x01}))
	if state != Triggered {
		t.Errorf("want Triggered, got %s", state)
	}
}

func TestParseTriggered_LittleEndian(t *testing.T) {
	state := poll(t, wrapOctetString([]byte{0x01, 0x00, 0x00, 0x00}))
	if state != Triggered {
		t.Errorf("want Triggered, got %s", state)
	}
}

func TestParseReady_BigEndian(t *testing.T) {
	state := poll(t, wrapOctetString([]byte{0x00, 0x00, 0x00, 0x02}))
	if state != Ready {
		t.Errorf("want Ready, got %s", state)
	}
}

func TestParseReady_LittleEndian(t *testing.T) {
	state := poll(t, wrapOctetString([]byte{0x02, 0x00, 0x00, 0x00}))
	if state != Ready {
		t.Errorf("want Ready, got %s", state)
	}
}

func TestNoSuchNameReturnsIdle(t *testing.T) {
	state := poll(t, wrapNoSuchName())
	if state != Idle {
		t.Errorf("want Idle for noSuchName, got %s", state)
	}
}

func TestTimeoutReturnsIdle(t *testing.T) {
	// Start a server that never replies
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := conn.LocalAddr().(*net.UDPAddr).Port

	state, _ := pollOnPort("127.0.0.1", 255, 100*time.Millisecond, p)
	if state != Idle {
		t.Errorf("want Idle on timeout, got %s", state)
	}
}

func TestInstanceIDInRequest(t *testing.T) {
	// OID arc 17 encodes as byte 0x11
	req := buildGetRequest(17)
	found := false
	for _, b := range req {
		if b == 0x11 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected byte 0x11 (arc 17) in request")
	}
	// OID arc 255 must NOT appear as raw 0xff — it encodes as 0x81 0x7f
	for _, b := range req {
		if b == 0xff {
			t.Error("unexpected 0xff byte in request (OID encoding bug)")
		}
	}
}

func TestEncodeOIDMultiByte(t *testing.T) {
	// Arc 236 encodes as 0x81 0x6c in base-128
	oid := encodeOID([]int{1, 3, 6, 1, 4, 1, 236})
	found := false
	for i := 0; i < len(oid)-1; i++ {
		if oid[i] == 0x81 && oid[i+1] == 0x6c {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 0x81 0x6c for arc 236, got: %x", oid)
	}
}

func TestParseResponseCapturedInstance4(t *testing.T) {
	if got := parseResponse(capturedInstance4Idle); !bytesEqual(got, []byte{0x00, 0x00, 0x00, 0x00}) {
		t.Errorf("want 00 00 00 00, got % x", got)
	}
}

func TestParseResponseEveryInstanceID(t *testing.T) {
	// The value must come from the varbind, never from bytes inside the OID.
	for id := 1; id <= 300; id++ {
		for _, raw := range [][]byte{{0x00, 0x00, 0x00, 0x00}, {0x01, 0x00, 0x00, 0x00}, {0x00, 0x00, 0x00, 0x02}} {
			if got := parseResponse(wrapResponse(id, 0x00, 0x00, tlv(0x04, raw))); !bytesEqual(got, raw) {
				t.Fatalf("instanceID %d: want % x, got % x", id, raw, got)
			}
		}
	}
}

func TestParseResponseTruncated(t *testing.T) {
	for n := 0; n < len(capturedInstance4Idle); n++ {
		if got := parseResponse(capturedInstance4Idle[:n]); got != nil {
			t.Fatalf("truncated to %d bytes: want nil, got % x", n, got)
		}
	}
}
