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
	nextPage255    = func() []byte { b := make([]byte, 255); b[0] = 0xa8; b[1] = 0x00; return b }()
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

func makeJPEGChunk(size int) []byte {
	chunk := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, size)...)
	return append(chunk, 0xff, 0xd9)
}

func recvExactTest(conn net.Conn, n int) []byte {
	buf := make([]byte, n)
	io.ReadFull(conn, buf)
	return buf
}

// --- ProtocolServer: printer-side mock ---

// ProtocolServer simulates the printer TCP protocol.
// pages[i][j] is chunk j of page i. Between pages, sends nextPage255 (0x00).
// After the last page, sends noMorePages255 (0x04).
type ProtocolServer struct {
	pages    [][][]byte
	received [][]byte
	listener net.Listener
	done     chan struct{}
}

func newProtocolServer(t *testing.T, chunks [][]byte) *ProtocolServer {
	t.Helper()
	if chunks == nil {
		chunks = [][]byte{makeJPEGChunk(400)}
	}
	return newMultiPageServer(t, [][][]byte{chunks})
}

func newMultiPageServer(t *testing.T, pages [][][]byte) *ProtocolServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &ProtocolServer{
		pages:    pages,
		listener: ln,
		done:     make(chan struct{}),
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
	s.record(recvExactTest(conn, 255)) // PARAMS (initial)
	conn.Write(caps255)
	s.record(recvExactTest(conn, 255)) // EXTRA
	conn.Write(extra255)
	s.record(recvExactTest(conn, 25))  // DIMS
	conn.Write(ack32)
	s.record(recvExactTest(conn, 4))   // READY
	conn.Write(ack32)

	for pageIdx, pageChunks := range s.pages {
		// POLL / FETCH loop for this page
		for i, chunk := range pageChunks {
			isLast := i == len(pageChunks)-1
			s.record(recvExactTest(conn, 4)) // POLL
			conn.Write(makeChunkStatus(len(chunk), isLast))
			s.record(recvExactTest(conn, 4)) // FETCH
			conn.Write(chunk)
		}

		// Next-page check response
		s.record(recvExactTest(conn, 255)) // PARAMS (next-page)
		if pageIdx == len(s.pages)-1 {
			conn.Write(noMorePages255) // 0x04 — no more pages
		} else {
			conn.Write(nextPage255) // 0x00 — next page ready
		}
	}

	s.record(recvExactTest(conn, 4)) // END
	conn.Write(ack32)
	s.record(recvExactTest(conn, 4)) // DISC
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

func download(t *testing.T, srv *ProtocolServer, resolution int) [][]byte {
	t.Helper()
	pages, err := downloadOnPort("127.0.0.1", resolution, srv.Port())
	if err != nil {
		t.Fatalf("download error: %v", err)
	}
	return pages
}

// downloadOnPort mirrors tcp.Download with a configurable port for testing.
func downloadOnPort(ip string, resolution, tcpPort int) ([][]byte, error) {
	addr := net.JoinHostPort(ip, itoa(tcpPort))

	probeConn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := handshake(probeConn); err != nil {
		probeConn.Close()
		return nil, err
	}
	probeConn.Close()

	dataConn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer dataConn.Close()

	if err := handshake(dataConn); err != nil {
		return nil, err
	}

	send(dataConn, magicRequest)
	recvExact(dataConn, 32)

	params := buildParams(resolution)
	send(dataConn, params)
	recvExact(dataConn, 255)

	extra := make([]byte, 255)
	copy(extra, magicExtra)
	send(dataConn, extra)
	recvExact(dataConn, 255)

	dims := append(append([]byte{}, magicDims...), dimsA4...)
	send(dataConn, dims)
	recvExact(dataConn, 32)

	send(dataConn, magicReady)
	recvExact(dataConn, 32)

	var pages [][]byte
	for {
		var chunks [][]byte
		for {
			send(dataConn, magicPoll)
			status, _ := recvExact(dataConn, 32)
			if status[1] != 0x00 {
				continue
			}
			chunkSize := int(binary.BigEndian.Uint16(status[6:8]))
			isLast := status[3] == 0x81
			send(dataConn, magicFetch)
			chunk, _ := recvExact(dataConn, chunkSize)
			chunks = append(chunks, chunk)
			if isLast {
				break
			}
		}

		raw := make([]byte, 0)
		for _, c := range chunks {
			raw = append(raw, c...)
		}
		pages = append(pages, raw)

		morePages := false
		for {
			send(dataConn, params)
			nextStatus, _ := recvExact(dataConn, 255)
			if nextStatus[1] == 0x04 {
				break
			}
			if nextStatus[1] == 0x00 {
				morePages = true
				break
			}
		}
		if !morePages {
			break
		}
	}

	send(dataConn, magicEnd)
	recvExact(dataConn, 32)
	send(dataConn, magicDisc)
	recvExact(dataConn, 32)

	return pages, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
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
	if string(srv.received[0]) != string(magicInfo) {
		t.Errorf("want %x, got %x", magicInfo, srv.received[0])
	}
}

func TestHandshakeSends255ByteProbe(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
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
	if string(srv.received[8]) != string(magicRequest) {
		t.Errorf("want %x, got %x", magicRequest, srv.received[8])
	}
}

func TestScanParamsResolution(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 150)
	srv.Join()
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
	chunk := makeJPEGChunk(100)
	srv := newProtocolServer(t, [][]byte{chunk})
	pages := download(t, srv, 300)
	srv.Join()
	if len(pages) != 1 {
		t.Fatalf("want 1 page, got %d", len(pages))
	}
	if string(pages[0]) != string(chunk) {
		t.Errorf("chunk mismatch: want %d bytes, got %d bytes", len(chunk), len(pages[0]))
	}
}

func TestReturnsMultipleChunksConcatenated(t *testing.T) {
	chunkA := makeJPEGChunk(50)
	chunkB := makeJPEGChunk(80)
	srv := newProtocolServer(t, [][]byte{chunkA, chunkB})
	pages := download(t, srv, 300)
	srv.Join()
	if len(pages) != 1 {
		t.Fatalf("want 1 page, got %d", len(pages))
	}
	want := append(chunkA, chunkB...)
	if string(pages[0]) != string(want) {
		t.Errorf("multi-chunk: want %d bytes, got %d bytes", len(want), len(pages[0]))
	}
}

func TestEndSequenceSent(t *testing.T) {
	srv := newProtocolServer(t, nil)
	download(t, srv, 300)
	srv.Join()
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

func TestMultiPageADFDownload(t *testing.T) {
	page1 := makeJPEGChunk(50)
	page2 := makeJPEGChunk(80)
	srv := newMultiPageServer(t, [][][]byte{{page1}, {page2}})
	pages := download(t, srv, 300)
	srv.Join()

	if len(pages) != 2 {
		t.Fatalf("want 2 pages, got %d", len(pages))
	}
	if string(pages[0]) != string(page1) {
		t.Errorf("page 1 mismatch: want %d bytes, got %d", len(page1), len(pages[0]))
	}
	if string(pages[1]) != string(page2) {
		t.Errorf("page 2 mismatch: want %d bytes, got %d", len(page2), len(pages[1]))
	}
}

func TestMultiPageUsesOneTCPConnection(t *testing.T) {
	page1 := makeJPEGChunk(50)
	page2 := makeJPEGChunk(50)
	srv := newMultiPageServer(t, [][][]byte{{page1}, {page2}})
	pages := download(t, srv, 300)
	srv.Join()

	// Both pages must arrive; a new TCP connection per page would fail because
	// the mock only accepts two connections total (probe + data).
	if len(pages) != 2 {
		t.Errorf("want 2 pages, got %d — possible extra TCP connection opened", len(pages))
	}
}
