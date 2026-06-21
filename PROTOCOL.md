# Samsung M2070W — TCP Port 9400 Binary Protocol

Reverse-engineered from Wireshark captures of the Samsung Windows EPM Scan2PC
client and verified against live traces of the Go daemon.
All byte offsets are 0-indexed. All multi-byte integers are big-endian.
All response sizes are fixed; there is no length prefix or framing.

---

## Overview

The binary image download protocol runs on TCP port 9400. Every scan uses two
sequential connections to that port:

1. **Probe connection** — handshake only, then close. Signals the printer that
   the client is reachable.
2. **Data connection** — full scan protocol from REQUEST through DISC.

All pages of an ADF multi-page scan share the **same data connection**. Opening
a new connection between pages causes the printer to respond with EOF.

---

## Command Reference

| Message | Direction | Size | Magic bytes | Notes |
|---------|-----------|------|-------------|-------|
| INFO | PC → printer | 4 B | `1b a8 12 00` | Sent 2× per connection |
| INFO resp | printer → PC | 70 B | `a8 00 43 ...` | Model name + capability data |
| PROBE | PC → printer | 255 B | `1b a8 13 fb` | Zero-padded to 255 B |
| PROBE resp | printer → PC | 255 B | `a8 00 00 00 00 f9 ...` | |
| REQUEST | PC → printer | 4 B | `1b a8 16 00` | Scan session start |
| REQUEST resp | printer → PC | 32 B | `a8 00 1d 00 ...` | ACK |
| PARAMS | PC → printer | 255 B | `1b a8 20 fb` | Resolution + colour; used at session start and as next-page probe |
| PARAMS resp | printer → PC | 255 B | `a8 {status} 00 ...` | Session ACK or next-page status |
| EXTRA | PC → printer | 255 B | `1b a8 25 fb` | Zero payload |
| EXTRA resp | printer → PC | 255 B | `a8 00 00 00 ...` | |
| DIMS | PC → printer | 25 B | `1b a8 24 13` | Scan area in 1200-DPI units |
| DIMS resp | printer → PC | 32 B | `a8 00 1d 00 ...` | Actual pixel dimensions |
| READY | PC → printer | 4 B | `1b a8 31 00` | Arm scan engine; required before every page |
| READY resp | printer → PC | 32 B | `a8 00 1d 00 ...` | ACK |
| POLL | PC → printer | 4 B | `1b a8 28 00` | Check whether next chunk is ready |
| POLL resp | printer → PC | 32 B | `a8 {status} 1d ...` | Chunk status + size |
| FETCH | PC → printer | 4 B | `1b a8 29 00` | Request next chunk |
| FETCH resp | printer → PC | *varies* | `ff d8 ff e0 ...` | JPEG strip data |
| END | PC → printer | 4 B | `1b a8 06 00` | End scan session |
| END resp | printer → PC | 32 B | `a8 00 1d 00 ...` | ACK |
| DISC | PC → printer | 4 B | `1b a8 17 00` | Disconnect |
| DISC resp | printer → PC | 32 B | `a8 00 1d 00 ...` | ACK |

---

## Handshake

Performed identically on both the probe and data connections. Run **twice** per
connection:

```
PC  → printer :  4 B    INFO        1b a8 12 00
printer → PC  :  70 B   device info
PC  → printer :  255 B  PROBE       1b a8 13 fb  +  251 zero bytes
printer → PC  :  255 B  PROBE resp
... repeat once more (2 rounds total) ...
```

### Device info response (70 bytes)

| Bytes | Content |
|-------|---------|
| 0–3 | `a8 00 43 10` — response marker |
| 4–23 | Model name, ASCII, space-padded to 20 chars ("Samsung M2070 Series") |
| 28–31 | Max scan width at 1200 DPI, uint32 BE (observed: 10208) |
| 32–35 | Max scan height at 1200 DPI, uint32 BE (observed: 14040) |
| 36–69 | Unknown capability flags |

---

## Session Setup (data connection only)

After the handshake, the client sends five commands before entering the POLL/FETCH
loop:

```
REQUEST → PARAMS → EXTRA → DIMS → READY → (POLL/FETCH)
```

### PARAMS packet (255 bytes)

```
offset 0–3  : 1b a8 20 fb   magic
offset 4–5  : uint16 BE     resolution in DPI  (e.g. 0x012c = 300)
offset 6    : 0x01          colour mode  (0x01 = colour)
offset 7–254: 00 ...        zero padding
```

The same 255-byte PARAMS packet is reused for the next-page probe after each page.

### DIMS packet (25 bytes)

The scan area is specified in 1200-DPI units regardless of the requested scan resolution.
The printer downsamples internally.

```
offset 0–3  : 1b a8 24 13   magic
offset 4–5  : 30 00         constant
offset 6–9  : uint32 BE     scan width at 1200 DPI
                             A4: 0x000026c4 = 9924  (8.27 in × 1200)
offset 10–13: uint32 BE     scan height at 1200 DPI
                             A4: 0x000036cc = 14028 (11.69 in × 1200)
offset 14–24:               constant  05 05 00 00 00 00 05 06 01 15 40 00
```

Full A4 DIMS packet (hex):
```
1b a8 24 13  30 00 00 26  c4 00 00 36  cc 05 05 00
00 00 00 05  06 01 15 40  00
```

### DIMS response (32 bytes)

The printer echoes back the actual pixel dimensions at the requested DPI:

```
offset 0–3  : a8 00 1d 00   response marker
offset 4–5  : 00 01         expected page count
offset 10–11: uint16 BE     scan width in pixels  (A4 at 300 DPI: 0x09b1 = 2481)
offset 14–15: uint16 BE     scan height in pixels (A4 at 300 DPI: 0x0db3 = 3507)
offset 17   : 06            unknown flag
offset 18–31: 00 ...        zeros
```

---

## POLL Status Response (32 bytes)

All POLL responses share this layout:

```
offset 0    : a8            response marker
offset 1    : status        0x08 = scanner busy (poll again)
                            0x00 = chunk ready (send FETCH)
                            0x04 = no more data
offset 2    : 1d            POLL marker — distinguishes this 32 B response
                            from a 255 B PARAMS response (where offset 2 = 0x00)
offset 3    : flags         Normal mode:    0x80 = more chunks, 0x81 = last chunk
                            Streaming mode: always 0x00
offset 4–5  : 00 00         zeros
offset 6–7  : uint16 BE     chunk_size — bytes in the next FETCH response
                            Normal mode:    2772–30372 B (non-zero)
                            Streaming mode: always 0x0000
offset 8–9  : uint16 BE     unknown (observed: 0x0020)
offset 10–11: uint16 BE     scan width in pixels (echoes DIMS response)
offset 12–13: 00 01         unknown flags
offset 14–31: 00 ...        zeros
```

---

## POLL/FETCH Loop — Normal Mode

**When:** `chunk_size > 0` in the POLL response.
**Used for:** page 1 on all firmware; all pages with `AppType=WIN`.

```
loop:
    POLL
    if status[1] == 0x08: continue          # scanner busy
    chunk_size = BigEndian.Uint16(status[6:8])
    is_last    = status[3] == 0x81
    FETCH → exactly chunk_size bytes
    if is_last: break
```

- `status[3] == 0x81` is the only end-of-page signal in normal mode. It is reliable
  and appears on the final POLL before the last FETCH.
- FETCH returns exactly `chunk_size` bytes: JPEG data ending with `ff d9`, followed
  by zero padding to fill the remainder. A single `recvExact(chunk_size)` reads
  both; no separate padding drain is needed.
- At 300 DPI A4 colour: ~110 strips, each 2481×32 px, ranging 2772–30372 B.

---

## POLL/FETCH Loop — Streaming Mode

**When:** `chunk_size == 0` in a `status[1] == 0x00` POLL response.
**Used for:** page 2 and beyond with `AppType=MAC`.

```
loop:
    POLL
    # status[1] == 0x00, chunk_size == 0x0000, status[3] == 0x00
    FETCH → read raw bytes until JPEG EOI (ff d9) inclusive
    recvExact(16)                             # drain post-EOI padding (always 16 B)
    set 2-second deadline on next POLL recv
```

### Post-EOI padding

Exactly 16 bytes follow every JPEG EOI marker in streaming mode. Content is
variable — any byte value may appear. The drain is unconditional.

### End-of-streaming detection

There is no explicit last-strip flag in streaming mode (`status[3]` is always `0x00`).
End of page is detected by timeout:

- After each strip, a **2-second deadline** is set on the next POLL recv.
- Normal inter-strip gap: ~90 ms — deadline never reached.
- After the last strip: the printer stops responding to POLL → 2-second deadline fires
  → treat as end-of-page.

At 300 DPI A4 colour: ~271 strips per page, ~90 ms/strip, ~24 s total.

---

## Next-Page Check (PARAMS Loop)

After the last FETCH of each page, send the same 255-byte PARAMS packet and read a
255-byte response. The response is identified by `response[2] == 0x00`
(vs `0x1d` for a 32-byte POLL response).

```
offset 0    : a8
offset 1    : next_status
              0x04 = no more pages  → proceed to END
              0x00 = next page ready → send READY + recv 32 B → re-enter POLL/FETCH
              0x08 (or other)       = printer still processing → send PARAMS again
```

### READY is required before every page

When `next_status == 0x00`, the sequence is:

1. Send READY (4 B: `1b a8 31 00`)
2. Receive 32-byte ACK — a POLL_STATUS with `chunk_size=0` confirming the scan
   engine is now armed.
3. Re-enter the POLL/FETCH loop on the same TCP connection.

Without READY, the printer's scan engine remains idle and returns placeholder JPEGs
on every FETCH indefinitely.

The ADF takes approximately 9 seconds to eject one page and feed the next.
Expect ~450 PARAMS exchanges returning `next_status=0x08` before `0x00` or `0x04`
appears.

---

## End Sequence

```
PC → printer :  END   (1b a8 06 00)    4 B
printer → PC :  ACK                    32 B
PC → printer :  DISC  (1b a8 17 00)    4 B
printer → PC :  ACK                    32 B
close TCP connection
```

Both END and DISC are mandatory. Do not close the connection before receiving
the DISC ACK.

---

## Full Protocol Flow (2-page ADF)

```
          CLIENT                           PRINTER
            │                               │
            │── probe TCP connect ──────────►│
            │◄── Handshake × 2 ─────────────│
            │── close ──────────────────────►│
            │                               │
            │── data TCP connect ───────────►│
            │◄── Handshake × 2 ─────────────│
            │── REQUEST (4 B) ──────────────►│
            │◄── ACK (32 B) ─────────────────│
            │── PARAMS (255 B) ─────────────►│
            │◄── ACK (255 B) ────────────────│
            │── EXTRA (255 B) ──────────────►│
            │◄── ACK (255 B) ────────────────│
            │── DIMS (25 B) ────────────────►│
            │◄── ACK (32 B, pixel dims) ─────│
            │── READY (4 B) ────────────────►│  arm scan engine
            │◄── ACK (32 B) ─────────────────│
            │                               │
            │  ┌── PAGE 1: NORMAL MODE ─────┐│
            │  │ POLL ──────────────────────►││  repeat while status[1]=0x08
            │  │◄── STATUS (32 B) ───────────││  chunk_size=K, flags=0x80/0x81
            │  │ FETCH ─────────────────────►││
            │  │◄── JPEG strip (K bytes) ────││
            │  │  ~110 strips; break on 0x81 ││
            │  └─────────────────────────────┘│
            │                               │
            │  ┌── BETWEEN PAGES ───────────┐│
            │  │ PARAMS ────────────────────►││  repeat while status[1]=0x08
            │  │◄── 255 B (status=0x08) ─────││  ADF feeding page 2 (~9 s)
            │  │ PARAMS ────────────────────►││
            │  │◄── 255 B (status=0x00) ─────││  page 2 ready
            │  │ READY (4 B) ───────────────►││  re-arm scan engine
            │  │◄── ACK (32 B) ──────────────││
            │  └─────────────────────────────┘│
            │                               │
            │  ┌── PAGE 2: STREAMING MODE ──┐│
            │  │ POLL ──────────────────────►││  status[1]=0x00, chunk_size=0
            │  │ FETCH ─────────────────────►││
            │  │◄── JPEG strip + 16 B pad ───││  boundary = ff d9
            │  │  ~271 strips               ││
            │  │ POLL ──────────────────────►││  2-second timeout → end-of-page
            │  └─────────────────────────────┘│
            │                               │
            │  ┌── AFTER ALL PAGES ─────────┐│
            │  │ PARAMS ────────────────────►││
            │  │◄── 255 B (status=0x04) ─────││  no more pages
            │  └─────────────────────────────┘│
            │                               │
            │── END (4 B) ──────────────────►│
            │◄── ACK (32 B) ─────────────────│
            │── DISC (4 B) ─────────────────►│
            │◄── ACK (32 B) ─────────────────│
            │── TCP close ───────────────────►│
```

---

## Timing Reference (2-page A4, 300 DPI, colour)

| Phase | Duration |
|-------|---------|
| Probe + data handshake + REQUEST/PARAMS/EXTRA/DIMS/READY | ~14 s |
| Page 1: initial busy polls (ADF warming) | ~1.5 s |
| Page 1: normal-mode strips (~110) | ~8 s |
| Between pages: PARAMS loop (ADF feeding page 2) | ~9 s |
| Page 2: streaming strips (~271 at ~90 ms/strip) | ~24 s |
| 2-second end-of-streaming timeout | 2 s |
| Final PARAMS (status=0x04) + END + DISC | ~0.5 s |
| **Total** | **~59 s** |

A 2-page scan takes approximately one minute. Do not kill the daemon before
a "scan saved" log message appears.

---

## Windows vs macOS Firmware

The firmware mode is selected by `AppType` in the HTTP S2PC_AppList POST
(see the parent-level `PROTOCOL.md` for the HTTP layer).

| Aspect | AppType=WIN | AppType=MAC |
|--------|-------------|-------------|
| Page 1 POLL/FETCH mode | Normal (chunk_size > 0) | Normal (same) |
| Page 2+ POLL/FETCH mode | Normal (chunk_size > 0) | **Streaming** (chunk_size = 0) |
| End-of-page 2+ signal | `status[3] == 0x81` | 2-second POLL timeout |
| Post-EOI padding | Consumed by `recvExact(chunk_size)` | Explicit 16-byte drain |
| Strip size at 300 DPI A4 | 2481×32 px | 2481×32 px |
| Strips per A4 page | ~110 | ~271 |
| READY before page 2+ | Required | Required |

The binary command set is identical between the two modes. The difference is
entirely in how FETCH responses are framed and how end-of-page is signalled.
