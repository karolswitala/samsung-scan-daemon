package tcp

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	port    = 9400
	timeout = 30 * time.Second
)

var (
	magicInfo    = []byte{0x1b, 0xa8, 0x12, 0x00} // device info request (4B)
	magicProbe   = []byte{0x1b, 0xa8, 0x13, 0xfb} // session probe header (padded to 255B)
	magicRequest = []byte{0x1b, 0xa8, 0x16, 0x00} // scan start request (4B)
	magicParams  = []byte{0x1b, 0xa8, 0x20, 0xfb} // scan parameters header (255B)
	magicExtra   = []byte{0x1b, 0xa8, 0x25, 0xfb} // extra parameter block header (255B)
	magicDims    = []byte{0x1b, 0xa8, 0x24, 0x13} // scan area dimensions header (4B of 25B)
	magicReady   = []byte{0x1b, 0xa8, 0x31, 0x00} // ready to receive (4B)
	magicPoll    = []byte{0x1b, 0xa8, 0x28, 0x00} // poll for next chunk (4B)
	magicFetch   = []byte{0x1b, 0xa8, 0x29, 0x00} // fetch chunk (4B)
	magicEnd     = []byte{0x1b, 0xa8, 0x06, 0x00} // end scan (4B)
	magicDisc    = []byte{0x1b, 0xa8, 0x17, 0x00} // disconnect (4B)

	// A4 scan area at 1200 DPI base: 8.27"×1200=9924=0x26C4, 11.69"×1200=14028=0x36CC
	dimsA4 = mustDecodeHex("30000026c4000036cc050500000000050601154000") // 21 bytes
)

func mustDecodeHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := range b {
		hi := hexVal(s[2*i])
		lo := hexVal(s[2*i+1])
		b[i] = hi<<4 | lo
	}
	return b
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

func recvExact(conn net.Conn, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(conn, buf)
	return buf, err
}

func send(conn net.Conn, data []byte) error {
	_, err := conn.Write(data)
	return err
}

// handshake performs two rounds of: 4B info → 70B response → 255B probe → 255B response.
func handshake(conn net.Conn) error {
	for i := 0; i < 2; i++ {
		if err := send(conn, magicInfo); err != nil {
			return fmt.Errorf("handshake send INFO#%d: %w", i, err)
		}
		if _, err := recvExact(conn, 70); err != nil {
			return fmt.Errorf("handshake recv INFO#%d: %w", i, err)
		}
		probe := make([]byte, 255)
		copy(probe, magicProbe)
		if err := send(conn, probe); err != nil {
			return fmt.Errorf("handshake send PROBE#%d: %w", i, err)
		}
		if _, err := recvExact(conn, 255); err != nil {
			return fmt.Errorf("handshake recv PROBE#%d: %w", i, err)
		}
	}
	return nil
}

// buildParams constructs the 255-byte scan parameter packet.
// Resolution is encoded as uint16 big-endian at offset 4; byte 6 = 0x01 (color).
func buildParams(resolution int) []byte {
	pkt := make([]byte, 255)
	copy(pkt, magicParams)
	binary.BigEndian.PutUint16(pkt[4:6], uint16(resolution))
	pkt[6] = 0x01 // color
	return pkt
}

// Download runs the Samsung Scan-to-PC TCP protocol and returns one page of raw JPEG data.
// hasNextPage is true if the printer signalled that more pages are available.
func Download(ip string, resolution int) (pageBytes []byte, hasNextPage bool, err error) {
	addr := fmt.Sprintf("%s:%d", ip, port)

	// Probe connection — verify scanner is alive
	probeConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, false, fmt.Errorf("probe connect: %w", err)
	}
	probeConn.SetDeadline(time.Now().Add(timeout))
	if err := handshake(probeConn); err != nil {
		probeConn.Close()
		return nil, false, fmt.Errorf("probe handshake: %w", err)
	}
	probeConn.Close()

	// Data connection
	dataConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, false, fmt.Errorf("data connect: %w", err)
	}
	defer dataConn.Close()
	dataConn.SetDeadline(time.Now().Add(timeout))

	if err := handshake(dataConn); err != nil {
		return nil, false, fmt.Errorf("data handshake: %w", err)
	}

	// REQUEST
	if err := send(dataConn, magicRequest); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, fmt.Errorf("recv REQUEST ack: %w", err)
	}

	// PARAMS
	if err := send(dataConn, buildParams(resolution)); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, false, fmt.Errorf("recv PARAMS ack: %w", err)
	}

	// EXTRA (all zeros)
	extra := make([]byte, 255)
	copy(extra, magicExtra)
	if err := send(dataConn, extra); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, false, fmt.Errorf("recv EXTRA ack: %w", err)
	}

	// DIMS (A4 hardcoded)
	dims := append(append([]byte{}, magicDims...), dimsA4...)
	if err := send(dataConn, dims); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, fmt.Errorf("recv DIMS ack: %w", err)
	}

	// READY
	if err := send(dataConn, magicReady); err != nil {
		return nil, false, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, false, fmt.Errorf("recv READY ack: %w", err)
	}

	// POLL / FETCH loop
	var chunks [][]byte
	for {
		if err := send(dataConn, magicPoll); err != nil {
			return nil, false, err
		}
		status, err := recvExact(dataConn, 32)
		if err != nil {
			return nil, false, fmt.Errorf("recv POLL status: %w", err)
		}

		if status[1] != 0x00 {
			// Scanner busy — keep polling
			continue
		}

		chunkSize := int(binary.BigEndian.Uint16(status[6:8]))
		isLast := status[3] == 0x81

		if err := send(dataConn, magicFetch); err != nil {
			return nil, false, err
		}
		chunk, err := recvExact(dataConn, chunkSize)
		if err != nil {
			return nil, false, fmt.Errorf("recv chunk: %w", err)
		}
		chunks = append(chunks, chunk)

		if isLast {
			break
		}
	}

	// Next-page check: send PARAMS until printer signals no more pages (status[1]==0x04)
	params := buildParams(resolution)
	for {
		if err := send(dataConn, params); err != nil {
			return nil, false, err
		}
		nextStatus, err := recvExact(dataConn, 255)
		if err != nil {
			return nil, false, fmt.Errorf("recv next-page status: %w", err)
		}
		if nextStatus[1] == 0x04 {
			hasNextPage = false
			break
		}
		// Printer signalled another page is ready on this connection.
		// Return current page so caller can collect it; caller will invoke Download again.
		// Note: true multi-page on one TCP connection requires re-entering the POLL/FETCH
		// loop without a new TCP handshake — flagged here for future hardware testing.
		hasNextPage = true
		break
	}

	// END + DISC
	if err := send(dataConn, magicEnd); err != nil {
		return nil, hasNextPage, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, hasNextPage, fmt.Errorf("recv END ack: %w", err)
	}
	if err := send(dataConn, magicDisc); err != nil {
		return nil, hasNextPage, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, hasNextPage, fmt.Errorf("recv DISC ack: %w", err)
	}

	result := make([]byte, 0)
	for _, c := range chunks {
		result = append(result, c...)
	}
	return result, hasNextPage, nil
}
