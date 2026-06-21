// Package tcp implements the Samsung M2070W binary image download protocol on
// TCP port 9400.
//
// # Connection sequence
//
// Every scan uses two sequential TCP connections to the same port:
//
//  1. Probe connection — handshake only, then close. Signals the printer that
//     the client is reachable and arms the scan engine.
//
//  2. Data connection — full protocol:
//     REQUEST → PARAMS → EXTRA → DIMS → READY → POLL/FETCH loop
//     → PARAMS (next-page check) → READY (re-arm) → POLL/FETCH loop → … → END → DISC
//
// Each connection starts with two rounds of the same handshake:
//
//	PC → printer:  4 B   INFO request   (1b a8 12 00)
//	printer → PC:  70 B  device info    (model name, capabilities)
//	PC → printer:  255 B PROBE          (1b a8 13 fb + zeros)
//	printer → PC:  255 B PROBE response
//
// # POLL / FETCH loop
//
// After READY the client polls until the scanner has a chunk ready:
//
//	POLL (4 B) → 32 B status
//	  status[1] == 0x08: busy — poll again
//	  status[1] == 0x00: chunk ready
//	    chunk_size = BigEndian.Uint16(status[6:8])
//	    is_last    = status[3] == 0x81
//	  FETCH (4 B) → chunk_size bytes of raw JPEG strip data
//
// Each chunk is one independently-decodable JPEG covering 32 scan lines.
// Chunks for a single page are concatenated and passed to imageutil.AssembleStrips.
//
// # Mac firmware streaming mode (page 2+)
//
// On macOS-targeted firmware (Samsung M2070W), page 2 and beyond use a
// streaming variant where chunk_size is always 0x0000 in the POLL status
// and status[3] is 0x00 for every strip (never 0x81 "last strip").
// The protocol is otherwise identical — FETCH still triggers each strip —
// but the strip size is not announced upfront. Instead the client reads raw
// bytes until the JPEG EOI marker (0xff 0xd9). The printer appends a small
// number of padding bytes after each EOI before the next status block.
//
// End-of-streaming detection: after each strip the client sets a 2-second
// deadline on the next POLL response. Normal strips reply within ~100 ms;
// when there are no more strips the printer stops answering and the deadline
// fires, signalling end-of-page. Additionally, status[2] != 0x1d or
// status[1] == 0x04 in the POLL response are treated as explicit signals.
//
// # Multi-page ADF
//
// After the last chunk of each page the client sends PARAMS (255 B) and reads
// a 255-byte next-page status:
//
//	status[1] == 0x04: no more pages → END + DISC
//	status[1] == 0x00: next page ready → send READY (4 B) → recv 32 B ack
//	                   → re-enter POLL/FETCH on the same connection
//	other:             printer still processing → send PARAMS again
//
// The READY (0x31) command is required for every page, not just page 1. Without
// it the printer does not arm the scan engine and returns blank dummy JPEGs.
//
// All pages are downloaded on the same data connection. Opening a new TCP
// connection for each page causes the printer to respond with EOF.
//
// # Scan area
//
// The DIMS packet encodes the scan area in 1200-DPI units regardless of the
// requested resolution. A4 values are hardcoded (8.27" × 11.69"):
//
//	width  = 9924  (0x26C4)
//	height = 14028 (0x36CC)
package tcp

import (
	"bufio"
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

	maxStripBytes = 5 << 20   // 5 MB per JPEG strip
	maxPageBytes  = 150 << 20 // 150 MB per page (A4 @1200 DPI)
	maxTotalBytes = 500 << 20 // 500 MB per scan (ADF multi-page)
	maxStrips     = 2000      // strips per page
	maxPages      = 50        // pages per scan
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

func recvExact(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func send(conn net.Conn, data []byte) error {
	_, err := conn.Write(data)
	return err
}

// readJPEGStrip reads raw bytes from br until JPEG EOI (0xff 0xd9) inclusive.
// Used in the Mac firmware streaming mode where the strip size is not announced
// upfront (chunkSize=0) and the strip boundary is marked by the EOI marker.
func readJPEGStrip(br *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		// ReadBytes(0xff) reads up to and including the next 0xff byte efficiently.
		chunk, err := br.ReadBytes(0xff)
		data = append(data, chunk...)
		if len(data) > maxStripBytes {
			return nil, fmt.Errorf("JPEG strip exceeds %d bytes", maxStripBytes)
		}
		if err != nil {
			return data, err
		}
		// Check if the byte after 0xff is 0xd9 (JPEG EOI).
		next, err := br.ReadByte()
		if err != nil {
			return data, err
		}
		data = append(data, next)
		if next == 0xd9 {
			return data, nil
		}
	}
}

// drainPostEOIPadding discards the 16 padding bytes the printer appends after
// each JPEG EOI in streaming mode. The padding is always exactly 16 bytes with
// variable content — it can contain any byte value including 0xa8.
func drainPostEOIPadding(conn net.Conn, br *bufio.Reader) {
	conn.SetDeadline(time.Now().Add(timeout))
	recvExact(br, 16)
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

	// Wrap the data connection in a buffered reader for efficient byte-level reads
	// (needed for JPEG EOI detection in streaming mode).
	br := bufio.NewReaderSize(dataConn, 65536)

	// REQUEST
	if err := send(dataConn, magicRequest); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 32); err != nil {
		return nil, fmt.Errorf("recv REQUEST ack: %w", err)
	}

	// PARAMS
	params := buildParams(resolution)
	if err := send(dataConn, params); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 255); err != nil {
		return nil, fmt.Errorf("recv PARAMS ack: %w", err)
	}

	// EXTRA (all zeros)
	extra := make([]byte, 255)
	copy(extra, magicExtra)
	if err := send(dataConn, extra); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 255); err != nil {
		return nil, fmt.Errorf("recv EXTRA ack: %w", err)
	}

	// DIMS (A4 hardcoded)
	dims := append(append([]byte{}, magicDims...), dimsA4...)
	if err := send(dataConn, dims); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 32); err != nil {
		return nil, fmt.Errorf("recv DIMS ack: %w", err)
	}

	// READY
	if err := send(dataConn, magicReady); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 32); err != nil {
		return nil, fmt.Errorf("recv READY ack: %w", err)
	}

	// Page download loop — stays on same TCP connection for all pages.
	// After each page's chunks, the next-page check determines whether to
	// re-enter the POLL/FETCH loop or proceed to END.
	totalBytes := 0
	for pageNum := 1; ; pageNum++ {
		// Reset deadline for each page so page N+1 gets a full timeout window.
		// Without this, a single 30-second window is shared across all pages;
		// page 1 consumes most of it and page 2 times out before it can complete.
		dataConn.SetDeadline(time.Now().Add(timeout))

		// POLL / FETCH loop for current page
		var chunks [][]byte
		pageBytes := 0
		streamingActive := false // true once chunkSize=0 strips are seen on this page
		for {
			if err := send(dataConn, magicPoll); err != nil {
				return nil, fmt.Errorf("page %d POLL: %w", pageNum, err)
			}
			status, err := recvExact(br, 32)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() && streamingActive {
					// 2 s streaming deadline expired: the printer stopped responding
					// to POLL after the last strip. Proceed to the next-page check.
					slog.Info("streaming done", "page", pageNum, "strips", len(chunks))
					dataConn.SetDeadline(time.Now().Add(timeout))
					break
				}
				return nil, fmt.Errorf("page %d recv POLL status: %w", pageNum, err)
			}
			dataConn.SetDeadline(time.Now().Add(timeout))

			// status[2] == 0x1d is the fixed marker for a 32 B POLL status response.
			// If the printer instead replies with a 255 B PARAMS-style packet (e.g. to
			// signal end-of-streaming), drain the remaining 223 bytes and break.
			if status[2] != 0x1d {
				slog.Debug("streaming page done (non-POLL response)", "page", pageNum,
					"status1", fmt.Sprintf("0x%02x", status[1]), "strips", len(chunks))
				if _, err := recvExact(br, 255-32); err != nil {
					slog.Warn("drain 255B response tail", "page", pageNum, "err", err)
				}
				break
			}

			if status[1] == 0x04 {
				// Printer explicitly signals end-of-page via the POLL status byte.
				slog.Debug("streaming page done (status[1]=0x04)", "page", pageNum, "strips", len(chunks))
				break
			}
			if status[1] != 0x00 {
				continue // scanner busy (0x08) — keep polling
			}

			chunkSize := int(binary.BigEndian.Uint16(status[6:8]))
			isLast := status[3] == 0x81

			if chunkSize == 0 {
				// Mac firmware streaming mode: printer returns chunkSize=0 for every
				// strip instead of announcing the size upfront. FETCH triggers the strip;
				// read raw bytes until JPEG EOI (0xff 0xd9); drain any post-EOI padding.
				if !streamingActive {
					slog.Info("streaming mode", "page", pageNum)
				}
				streamingActive = true
				slog.Debug("streaming strip", "page", pageNum, "strip", len(chunks)+1, "isLast", isLast)
				if err := send(dataConn, magicFetch); err != nil {
					return nil, fmt.Errorf("page %d streaming FETCH: %w", pageNum, err)
				}
				strip, err := readJPEGStrip(br)
				if err != nil {
					return nil, fmt.Errorf("page %d streaming read: %w", pageNum, err)
				}
				chunks = append(chunks, strip)
				pageBytes += len(strip)
				if len(chunks) > maxStrips {
					return nil, fmt.Errorf("page %d exceeds %d strips", pageNum, maxStrips)
				}
				if pageBytes > maxPageBytes {
					return nil, fmt.Errorf("page %d exceeds %d bytes", pageNum, maxPageBytes)
				}
				if len(chunks)%25 == 0 {
					slog.Info("streaming", "page", pageNum, "strips", len(chunks))
				}
				drainPostEOIPadding(dataConn, br)
				if isLast {
					break
				}
				// Short deadline for next POLL: normal strips arrive within ~100 ms;
				// if there are no more strips the printer won't respond.
				dataConn.SetDeadline(time.Now().Add(2 * time.Second))
				continue
			}

			if err := send(dataConn, magicFetch); err != nil {
				return nil, fmt.Errorf("page %d FETCH: %w", pageNum, err)
			}
			chunk, err := recvExact(br, chunkSize)
			if err != nil {
				return nil, fmt.Errorf("page %d recv chunk: %w", pageNum, err)
			}
			chunks = append(chunks, chunk)
			pageBytes += len(chunk)
			if len(chunks) > maxStrips {
				return nil, fmt.Errorf("page %d exceeds %d strips", pageNum, maxStrips)
			}
			if pageBytes > maxPageBytes {
				return nil, fmt.Errorf("page %d exceeds %d bytes", pageNum, maxPageBytes)
			}

			if isLast {
				break
			}
		}

		// If the POLL loop exited with no strips the printer had no data for this
		// page — either a premature PARAMS 0x00 or a stream misalignment. Entering
		// the PARAMS next-page check here causes an immediate morePages=true loop
		// that makes the printer LCD show "scan another page?" without scanning.
		if len(chunks) == 0 {
			slog.Warn("page had no strips — stopping scan", "page", pageNum)
			break
		}

		if len(pages) >= maxPages {
			return nil, fmt.Errorf("scan exceeds %d pages", maxPages)
		}
		raw := make([]byte, 0)
		for _, c := range chunks {
			raw = append(raw, c...)
		}
		totalBytes += len(raw)
		if totalBytes > maxTotalBytes {
			return nil, fmt.Errorf("scan exceeds %d total bytes", maxTotalBytes)
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
			nextStatus, err := recvExact(br, 255)
			if err != nil {
				return nil, fmt.Errorf("page %d recv next-page status: %w", pageNum, err)
			}
			slog.Debug("next-page", "page", pageNum, "status", fmt.Sprintf("0x%02x", nextStatus[1]))
			if nextStatus[1] == 0x04 {
				break // no more pages
			}
			if nextStatus[1] == 0x00 {
				// Re-arm the scanner for the next page. Without this the printer
				// never starts scanning and returns blank dummy JPEGs indefinitely.
				// Confirmed by Windows and Mac traces: both send READY (0x31) here.
				if err := send(dataConn, magicReady); err != nil {
					return nil, fmt.Errorf("page %d next-page READY: %w", pageNum, err)
				}
				if _, err := recvExact(br, 32); err != nil {
					return nil, fmt.Errorf("page %d next-page READY ack: %w", pageNum, err)
				}
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
	if _, err := recvExact(br, 32); err != nil {
		return nil, fmt.Errorf("recv END ack: %w", err)
	}
	if err := send(dataConn, magicDisc); err != nil {
		return nil, err
	}
	if _, err := recvExact(br, 32); err != nil {
		return nil, fmt.Errorf("recv DISC ack: %w", err)
	}

	return pages, nil
}
