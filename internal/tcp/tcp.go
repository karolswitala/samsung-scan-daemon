package tcp

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
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

// Download runs the Samsung Scan-to-PC TCP protocol and returns all pages as raw JPEG data.
// Single-page scans return a one-element slice. ADF multi-page scans return one entry per page,
// all downloaded over the same TCP connection without reconnecting between pages.
func Download(ip string, resolution int) (pages [][]byte, err error) {
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	// Probe connection — verify scanner is alive
	probeConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("probe connect: %w", err)
	}
	probeConn.SetDeadline(time.Now().Add(timeout))
	if err := handshake(probeConn); err != nil {
		probeConn.Close()
		return nil, fmt.Errorf("probe handshake: %w", err)
	}
	probeConn.Close()

	// Data connection
	dataConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("data connect: %w", err)
	}
	defer dataConn.Close()
	dataConn.SetDeadline(time.Now().Add(timeout))

	if err := handshake(dataConn); err != nil {
		return nil, fmt.Errorf("data handshake: %w", err)
	}

	// REQUEST
	if err := send(dataConn, magicRequest); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, fmt.Errorf("recv REQUEST ack: %w", err)
	}

	// PARAMS
	params := buildParams(resolution)
	if err := send(dataConn, params); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, fmt.Errorf("recv PARAMS ack: %w", err)
	}

	// EXTRA (all zeros)
	extra := make([]byte, 255)
	copy(extra, magicExtra)
	if err := send(dataConn, extra); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 255); err != nil {
		return nil, fmt.Errorf("recv EXTRA ack: %w", err)
	}

	// DIMS (A4 hardcoded)
	dims := append(append([]byte{}, magicDims...), dimsA4...)
	if err := send(dataConn, dims); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, fmt.Errorf("recv DIMS ack: %w", err)
	}

	// READY
	if err := send(dataConn, magicReady); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, fmt.Errorf("recv READY ack: %w", err)
	}

	// Page download loop — stays on same TCP connection for all pages.
	// After each page's chunks, the next-page check determines whether to
	// re-enter the POLL/FETCH loop or proceed to END.
	for pageNum := 1; ; pageNum++ {
		// POLL / FETCH loop for current page
		var chunks [][]byte
		for {
			if err := send(dataConn, magicPoll); err != nil {
				return nil, fmt.Errorf("page %d POLL: %w", pageNum, err)
			}
			status, err := recvExact(dataConn, 32)
			if err != nil {
				return nil, fmt.Errorf("page %d recv POLL status: %w", pageNum, err)
			}

			if status[1] != 0x00 {
				continue // scanner busy — keep polling
			}

			chunkSize := int(binary.BigEndian.Uint16(status[6:8]))
			isLast := status[3] == 0x81

			if err := send(dataConn, magicFetch); err != nil {
				return nil, fmt.Errorf("page %d FETCH: %w", pageNum, err)
			}
			chunk, err := recvExact(dataConn, chunkSize)
			if err != nil {
				return nil, fmt.Errorf("page %d recv chunk: %w", pageNum, err)
			}
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

		// Next-page check: keep sending PARAMS until the printer signals done or ready.
		//   0x04 — no more pages → proceed to END
		//   0x00 — next page ready → re-enter POLL/FETCH on the same connection
		//   other (e.g. 0x08) — printer still processing → send PARAMS again
		// The debug log records the exact byte seen, which is useful for verifying
		// ADF multi-page behavior during a real capture.
		morePages := false
		for {
			if err := send(dataConn, params); err != nil {
				return nil, fmt.Errorf("page %d next-page PARAMS: %w", pageNum, err)
			}
			nextStatus, err := recvExact(dataConn, 255)
			if err != nil {
				return nil, fmt.Errorf("page %d recv next-page status: %w", pageNum, err)
			}
			slog.Debug("next-page", "page", pageNum, "status", fmt.Sprintf("0x%02x", nextStatus[1]))
			if nextStatus[1] == 0x04 {
				break // no more pages
			}
			if nextStatus[1] == 0x00 {
				morePages = true
				break // next page ready — re-enter POLL/FETCH
			}
			// 0x08 or other: printer still processing — keep sending PARAMS
		}

		if !morePages {
			break
		}
	}

	// END + DISC
	if err := send(dataConn, magicEnd); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, fmt.Errorf("recv END ack: %w", err)
	}
	if err := send(dataConn, magicDisc); err != nil {
		return nil, err
	}
	if _, err := recvExact(dataConn, 32); err != nil {
		return nil, fmt.Errorf("recv DISC ack: %w", err)
	}

	return pages, nil
}
