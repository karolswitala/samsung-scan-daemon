package tcp

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// --- Canned response data (exactly N bytes, no prefix header) ---

var (
	deviceInfo70 = func() []byte {
		b := make([]byte, 70)
		copy(b, []byte{0xa8, 0x00, 0x43, 0x10})
		copy(b[4:], []byte("Samsung M2070 Series"))
		return b
	}()
	caps255        = makeResp255(0xa8, 0x00, 0x00, 0x00)
	extra255       = makeResp255(0xa8, 0x00, 0x00, 0x00)
	ack32          = makeAck32()
	noMorePages255 = func() []byte { b := make([]byte, 255); b[0] = 0xa8; b[1] = 0x04; return b }()
)

func makeResp255(b0, b1, b2, b3 byte) []byte {
	b := make([]byte, 255)
	b[0], b[1], b[2], b[3] = b0, b1, b2, b3
	return b
}

func makeAck32() []byte {
	b := make([]byte, 32)
	b[0] = 0xa8
	return b
}

func makeChunkStatus(chunkSize int, isLast bool) []byte {
	s := make([]byte, 32)
	s[0] = 0xa8
	s[1] = 0x00 // chunk ready
	if isLast {
		s[3] = 0x81
	} else {
		s[3] = 0x80
	}
	binary.BigEndian.PutUint16(s[6:8], uint16(chunkSize))
	return s
}

func recvExactTest(conn net.Conn, n int) []byte {
	buf := make([]byte, n)
	io.ReadFull(conn, buf)
	return buf
}

// --- ProtocolServer: printer-side mock ---

type ProtocolServer struct {
	jpeg_chunks [][]byte
	received    [][]byte
	listener    net.Listener
	done        chan struct{}
}

func newProtocolServer(t *testing.T, chunks [][]byte) *ProtocolServer {
	t.Helper()
	if chunks == nil {
		fakeJPEG := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 400)...)
		fakeJPEG = append(fakeJPEG, 0xff, 0xd9)
		chunks = [][]byte{fakeJPEG}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &ProtocolServer{
		jpeg_chunks: chunks,
		listener:    ln,
		done:        make(chan struct{}),
	}
	go srv.run()
	return srv
}

func (s *ProtocolServer) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *ProtocolServer) record(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	s.received = append(s.received, cp)
}

func (s *ProtocolServer) handshake(conn net.Conn) {
	for i := 0; i < 2; i++ {
		s.record(recvExactTest(conn, 4))   // INFO request
		conn.Write(deviceInfo70)
		s.record(recvExactTest(conn, 255)) // PROBE
		conn.Write(caps255)
	}
}

func (s *ProtocolServer) handleProbe(conn net.Conn) {
	s.handshake(conn)
}

func (s *ProtocolServer) handleData(conn net.Conn) {
	s.handshake(conn)

	s.record(recvExactTest(conn, 4))   // REQUEST
	conn.Write(ack32)

	s.record(recvExactTest(conn, 255)) // PARAMS
	conn.Write(caps255)

	s.record(recvExactTest(conn, 255)) // EXTRA
	conn.Write(extra255)

	s.record(recvExactTest(conn, 25))  // DIMS
	conn.Write(ack32)

	s.record(recvExactTest(conn, 4))   // READY
	conn.Write(ack32)

	// POLL / FETCH loop
	for i, chunk := range s.jpeg_chunks {
		isLast := i == len(s.jpeg_chunks)-1
		s.record(recvExactTest(conn, 4)) // POLL
		conn.Write(makeChunkStatus(len(chunk), isLast))
		s.record(recvExactTest(conn, 4)) // FETCH
		conn.Write(chunk)
	}

	// Next-page check
	s.record(recvExactTest(conn, 255)) // PARAMS (next-page)
	conn.Write(noMorePages255)

	s.record(recvExactTest(conn, 4))   // END
	conn.Write(ack32)
	s.record(recvExactTest(conn, 4))   // DISC
	conn.Write(ack32)
}

func (s *ProtocolServer) run() {
	defer close(s.done)
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	s.handleProbe(conn)
	conn.Close()

	conn, err = s.listener.Accept()
	if err != nil {
		return
	}
	s.handleData(conn)
	conn.Close()
}

func (s *ProtocolServer) Join() {
	<-s.done
	s.listener.Close()
}

func download(t *testing.T, srv *ProtocolServer, resolution int) []byte {
	t.Helper()
	// Override the port-level Download by dialing the mock directly
	data, _, err := downloadOnPort("127.0.0.1", resolution, srv.Port())
	if err != nil {
		t.Fatalf("download error: %v", err)
	}
	return data
}

// downloadOnPort is Download with a configurable port — test-only.
func downloadOnPort(ip string, resolution, tcpPort int) ([]byte, bool, error) {
	addr := net.JoinHostPort(ip, itoa(tcpPort))

	probeConn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, false, err
	}
	if err := handshake(probeConn); err != nil {
		probeConn.Close()
		return nil, false, err
	}
	probeConn.Close()

	dataConn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, false, err
	}
	defer dataConn.Close()

	if err := handshake(dataConn); err != nil {
		return nil, false, err
	}

	if err := send(dataConn, magicRequest); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, err
	}

	if err := send(dataConn, buildParams(resolution)); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, false, err
	}

	extra := make([]byte, 255)
	copy(extra, magicExtra)
	if err := send(dataConn, extra); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, false, err
	}

	dims := append(append([]byte{}, magicDims...), dimsA4...)
	if err := send(dataConn, dims); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, err
	}

	if err := send(dataConn, magicReady); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, err
	}

	var chunks [][]byte
	for {
		if err := send(dataConn, magicPoll); err != nil {
			return nil, false, err
		}
		status, err := recvExact(dataConn, 32)
		if err != nil {
			return nil, false, err
		}
		if status[1] != 0x00 {
			continue
		}
		chunkSize := int(binary.BigEndian.Uint16(status[6:8]))
		isLast := status[3] == 0x81
		if err := send(dataConn, magicFetch); err != nil {
			return nil, false, err
		}
		chunk, err := recvExact(dataConn, chunkSize)
		if err != nil {
			return nil, false, err
		}
		chunks = append(chunks, chunk)
		if isLast {
			break
		}
	}

	hasNextPage := false
	params := buildParams(resolution)
	if err := send(dataConn, params); err != nil {
		return nil, false, err
	}
	nextStatus, err := recvExact(dataConn, 255)
	if err != nil {
		return nil, false, err
	}
	if nextStatus[1] != 0x04 {
		hasNextPage = true
	}

	send(dataConn, magicEnd)
	recvExact(dataConn, 32)
	send(dataConn, magicDisc)
	recvExact(dataConn, 32)

	var result []byte
	for _, c := range chunks {
		result = append(result, c...)
	}
	return result, hasNextPage, nil
}

func itoa(n int) string {
	s := ""
	if n == 0 {
		return "0"
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// --- Tests ---

func TestProbeSendsCorrectMagic(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	// received[0] = probe info request
	if string(srv.received[0]) != string(magicInfo) {
		t.Errorf("want %x, got %x", magicInfo, srv.received[0])
	}
}

func TestHandshakeSends255ByteProbe(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	// received[1] = first probe packet (255B)
	if len(srv.received[1]) != 255 {
		t.Errorf("probe packet: want 255B, got %d", len(srv.received[1]))
	}
	if string(srv.received[1][:4]) != string(magicProbe) {
		t.Errorf("probe header: want %x, got %x", magicProbe, srv.received[1][:4])
	}
}

func TestScanRequestMagic(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	// Index 8 = data-connection REQUEST (after 4 probe + 4 data-handshake packets)
	if string(srv.received[8]) != string(magicRequest) {
		t.Errorf("want %x, got %x", magicRequest, srv.received[8])
	}
}

func TestScanParamsResolution(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 150)
	srv.Join()
	// received[9] = PARAMS (255B)
	params := srv.received[9]
	if string(params[:4]) != string(magicParams) {
		t.Errorf("params header: want %x, got %x", magicParams, params[:4])
	}
	res := int(binary.BigEndian.Uint16(params[4:6]))
	if res != 150 {
		t.Errorf("want resolution=150, got %d", res)
	}
}

func TestExtraParamsMagic(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	extra := srv.received[10]
	if len(extra) != 255 {
		t.Errorf("EXTRA: want 255B, got %d", len(extra))
	}
	if string(extra[:4]) != string(magicExtra) {
		t.Errorf("EXTRA header: want %x, got %x", magicExtra, extra[:4])
	}
}

func TestDimsMagic(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	dims := srv.received[11]
	if len(dims) != 25 {
		t.Errorf("DIMS: want 25B, got %d", len(dims))
	}
	if string(dims[:4]) != string(magicDims) {
		t.Errorf("DIMS header: want %x, got %x", magicDims, dims[:4])
	}
}

func TestReadySignal(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	if string(srv.received[12]) != string(magicReady) {
		t.Errorf("READY: want %x, got %x", magicReady, srv.received[12])
	}
}

func TestReturnsSingleChunk(t *testing.T) {
	chunk := append([]byte{0xff, 0xd8}, make([]byte, 100)...)
	chunk = append(chunk, 0xff, 0xd9)
	srv := newProtocolServer(t, [][]byte{chunk})
	result := download(t, srv, 300)
	srv.Join()
	if string(result) != string(chunk) {
		t.Errorf("chunk mismatch: want %d bytes, got %d bytes", len(chunk), len(result))
	}
}

func TestReturnsMultipleChunksConcatenated(t *testing.T) {
	chunkA := append([]byte{0xff, 0xd8}, make([]byte, 50)...)
	chunkA = append(chunkA, 0xff, 0xd9)
	chunkB := append([]byte{0xff, 0xd8}, make([]byte, 80)...)
	chunkB = append(chunkB, 0xff, 0xd9)
	srv := newProtocolServer(t, [][]byte{chunkA, chunkB})
	result := download(t, srv, 300)
	srv.Join()
	want := append(chunkA, chunkB...)
	if string(result) != string(want) {
		t.Errorf("multi-chunk: want %d bytes, got %d bytes", len(want), len(result))
	}
}

func TestEndSequenceSent(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
	// With one chunk: received[16]=END, received[17]=DISC
	n := len(srv.received)
	if n < 18 {
		t.Fatalf("too few packets received: %d", n)
	}
	if string(srv.received[n-2]) != string(magicEnd) {
		t.Errorf("END: want %x, got %x", magicEnd, srv.received[n-2])
	}
	if string(srv.received[n-1]) != string(magicDisc) {
		t.Errorf("DISC: want %x, got %x", magicDisc, srv.received[n-1])
	}
}
