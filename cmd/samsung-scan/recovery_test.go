package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karol/samsung-scan/internal/snmp"
)

// These tests drive the real run() loop against a mock printer, so the recovery
// path is exercised through the production registration code rather than a
// re-implementation of it. Only the two things a test cannot supply — a live
// SNMP responder on privileged port 161, and a GUI console session — are
// swapped out via the pollFn/isActive seams.
//
// The behavior under test was validated against real hardware on
// fix/reregister-and-per-user-migration (commits 32e4e83, eadc07f); these tests
// exist to prove the port onto this loop preserves it.

// --- mock printer ----------------------------------------------------------

// mockPrinter answers the Scan2PC CGI endpoint. Each ADD returns the next
// InstanceID from ids (repeating the last once exhausted), so a re-registration
// is distinguishable from the first registration by the ID the daemon adopts.
type mockPrinter struct {
	mu       sync.Mutex
	ids      []int
	addCalls int      // every ADD attempt, including rejected ones
	events   []string // ordered: "ADD", "ADD_REJECTED", "DELETE", "APPLIST"

	// failAdds rejects the first N ADD attempts, simulating a printer that is
	// reachable over HTTP but not yet willing to register us.
	failAdds int

	// added receives the InstanceID of each accepted ADD. Buffered so the
	// handler never blocks when a test isn't listening.
	added chan int
}

func newMockPrinter(t *testing.T, ids ...int) (*mockPrinter, *httptest.Server) {
	t.Helper()
	if len(ids) == 0 {
		t.Fatal("newMockPrinter needs at least one InstanceID")
	}
	mp := &mockPrinter{ids: ids, added: make(chan int, 16)}
	srv := httptest.NewServer(mp)
	t.Cleanup(srv.Close)
	return mp, srv
}

func (m *mockPrinter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := string(body)

	switch {
	case strings.Contains(s, `RegiType="ADD"`):
		m.mu.Lock()
		m.addCalls++
		n := m.addCalls
		id := m.ids[len(m.ids)-1]
		if n <= len(m.ids) {
			id = m.ids[n-1]
		}
		rejected := n <= m.failAdds
		if rejected {
			m.events = append(m.events, "ADD_REJECTED")
		} else {
			m.events = append(m.events, "ADD")
		}
		m.mu.Unlock()

		if rejected {
			io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><root><S2PC_Regi Result="DUPLICATE_USER"/></root>`)
			return
		}
		select {
		case m.added <- id:
		default:
		}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><root><S2PC_Regi Result="ADD_OK" InstanceID="%d"/></root>`, id)

	case strings.Contains(s, `RegiType="DELETE"`):
		m.mu.Lock()
		m.events = append(m.events, "DELETE")
		m.mu.Unlock()
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><root><S2PC_Regi Result="DELETE_OK"/></root>`)

	default: // PostAppList — the real printer never answers this one.
		m.mu.Lock()
		m.events = append(m.events, "APPLIST")
		m.mu.Unlock()
	}
}

// accepted returns how many ADDs the printer accepted.
func (m *mockPrinter) accepted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.events {
		if e == "ADD" {
			n++
		}
	}
	return n
}

func (m *mockPrinter) sequence() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

// deregisteredBetweenAdds reports whether a DELETE separates the first two
// accepted ADDs — i.e. the daemon cleared its old entry before re-registering,
// which is what keeps the printer from answering DUPLICATE_USER.
func (m *mockPrinter) deregisteredBetweenAdds() bool {
	seen := 0
	sawDelete := false
	for _, e := range m.sequence() {
		switch e {
		case "ADD":
			seen++
			if seen == 2 {
				return sawDelete
			}
		case "DELETE":
			if seen >= 1 {
				sawDelete = true
			}
		}
	}
	return false
}

// --- scripted SNMP ---------------------------------------------------------

type pollStep struct {
	state snmp.State
	err   error
}

func ok(s snmp.State) pollStep { return pollStep{state: s} }

// fail is one poll with no usable response — a powered-off printer, or one that
// came back having forgotten our registration.
func fail() pollStep { return pollStep{state: snmp.Idle, err: snmp.ErrNoResponse} }

type pollScript struct {
	steps  []pollStep
	pos    int
	polled []int // the instanceID run() asked about, per call
	done   func()
}

// poll matches the snmp.Poll signature so it can replace pollFn. When the script
// runs out it cancels the context, letting run() return without an arbitrary
// sleep in the test.
func (p *pollScript) poll(ip string, instanceID int, timeout time.Duration) (snmp.State, error) {
	p.polled = append(p.polled, instanceID)
	if p.pos >= len(p.steps) {
		p.done()
		return snmp.Idle, nil
	}
	s := p.steps[p.pos]
	p.pos++
	return s.state, s.err
}

func (p *pollScript) lastPolled(t *testing.T) int {
	t.Helper()
	if len(p.polled) == 0 {
		t.Fatal("run() never polled the printer")
	}
	return p.polled[len(p.polled)-1]
}

// installSeams points run() at the scripted poller and a always-active console,
// restoring the production implementations when the test ends. Tests using this
// must not call t.Parallel — the seams are package-level vars.
func installSeams(t *testing.T, script *pollScript) {
	t.Helper()
	origPoll, origActive := pollFn, isActive
	t.Cleanup(func() { pollFn, isActive = origPoll, origActive })
	pollFn = script.poll
	isActive = func() bool { return true }
}

// runScripted drives the real run() against the mock printer until the script is
// exhausted, with a wall-clock backstop so a regression cannot hang the suite.
func runScripted(t *testing.T, srv *httptest.Server, steps ...pollStep) *pollScript {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	script := &pollScript{steps: steps, done: cancel}
	installSeams(t, script)

	// srv.Listener.Addr() is host:port, which is exactly what httpclient
	// interpolates into "http://" + ip + path.
	if err := run(ctx, srv.Listener.Addr().String(), t.TempDir(), time.Millisecond, ""); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatal("run did not finish the poll script within 10s — the loop is stuck")
	}
	return script
}

// captureLogs redirects slog to a buffer at DEBUG level for the test's duration.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return &buf
}

// --- tests -----------------------------------------------------------------

// TestRunReRegistersAfterPowerCycle is the regression this branch exists for.
// A power-cycled printer drops its Scan2PC table, so polls stop returning a
// value for our OID. The daemon must notice, re-register, and — the part that
// actually matters — poll the NEW InstanceID afterwards.
func TestRunReRegistersAfterPowerCycle(t *testing.T) {
	mp, srv := newMockPrinter(t, 3, 7) // first ADD → 3, re-register → 7
	logged := captureLogs(t)

	script := runScripted(t, srv,
		ok(snmp.Idle), ok(snmp.Idle), // healthy
		fail(), fail(), fail(), // power-cycled: threshold is 3
	)

	// scripts/manual-tests/02-power-cycle-recovery.sh greps the log for these
	// two literals to decide whether the hardware run passed. Asserting them
	// here keeps that script working if the log wording is ever reworded.
	for _, want := range []string{`consecutive=3`, `msg="re-registered after outage"`} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log is missing %q, which 02-power-cycle-recovery.sh greps for — log:\n%s", want, logged.String())
		}
	}

	if got := mp.accepted(); got != 2 {
		t.Errorf("expected a re-registration (2 accepted ADDs), got %d — sequence %v", got, mp.sequence())
	}
	if !mp.deregisteredBetweenAdds() {
		t.Errorf("expected a DELETE before the re-register (else the printer can answer DUPLICATE_USER), sequence %v", mp.sequence())
	}
	if got := script.lastPolled(t); got != 7 {
		t.Errorf("expected polling to adopt the new instanceID 7 after re-registering, still polling %d", got)
	}
}

// TestRunDoesNotReRegisterOnBriefBlip guards the other direction: a couple of
// dropped UDP packets must not churn the registration, because re-registering
// makes "My Mac" flicker on the printer LCD.
func TestRunDoesNotReRegisterOnBriefBlip(t *testing.T) {
	mp, srv := newMockPrinter(t, 3, 99) // a second ADD would hand out 99

	script := runScripted(t, srv,
		ok(snmp.Idle),
		fail(), fail(), // two failures — one short of the threshold
		ok(snmp.Idle), ok(snmp.Idle),
	)

	if got := mp.accepted(); got != 1 {
		t.Errorf("a 2-poll blip must not re-register: expected 1 accepted ADD, got %d — sequence %v", got, mp.sequence())
	}
	for i, id := range script.polled {
		if id != 3 {
			t.Fatalf("poll %d used instanceID %d, expected the original 3 throughout", i, id)
		}
	}
}

// TestRunSurvivesPrinterDownAtStartup covers the printer being off or asleep
// when the agent launches. Under launchd a fatal exit here becomes a crash-loop
// that rarely catches the printer's brief wake windows, so run() must stay up
// and register once the printer answers.
func TestRunSurvivesPrinterDownAtStartup(t *testing.T) {
	mp, srv := newMockPrinter(t, 7)
	mp.failAdds = 1 // first registration attempt is rejected

	script := runScripted(t, srv, ok(snmp.Idle), ok(snmp.Idle))

	if got := mp.accepted(); got != 1 {
		t.Errorf("expected the daemon to retry and register once, got %d accepted ADDs — sequence %v", got, mp.sequence())
	}
	if got := script.lastPolled(t); got != 7 {
		t.Errorf("expected polling with the recovered instanceID 7, got %d", got)
	}
}

// TestRunThrottlesRegisterFailures covers a printer left switched off: the
// registration path retries every tick, and before this throttle each retry
// wrote a WARN to ~/Library/Logs/samsung-scan.log, which has no rotation. Only
// the first attempt of a run may be a WARN.
func TestRunThrottlesRegisterFailures(t *testing.T) {
	mp, srv := newMockPrinter(t, 7)
	mp.failAdds = 5 // printer rejects us for the first five ticks

	logged := captureLogs(t)

	runScripted(t, srv, ok(snmp.Idle), ok(snmp.Idle))

	if got := mp.accepted(); got != 1 {
		t.Fatalf("expected the daemon to keep retrying and eventually register, got %d accepted ADDs", got)
	}
	// The startup deregister also warns once against an unreachable printer, so
	// count only the registration warnings.
	warns := strings.Count(logged.String(), `level=WARN msg="register failed`)
	if warns != 1 {
		t.Errorf("expected exactly 1 register WARN across 5 failed attempts, got %d — log:\n%s", warns, logged.String())
	}
	if !strings.Contains(logged.String(), "level=DEBUG msg=\"register retry failed\"") {
		t.Errorf("expected the suppressed retries to still be visible at DEBUG — log:\n%s", logged.String())
	}
}

// TestRunHealthyNeverReRegisters is the baseline: with the printer answering
// normally the recovery path must stay completely out of the way.
func TestRunHealthyNeverReRegisters(t *testing.T) {
	mp, srv := newMockPrinter(t, 3, 99)

	script := runScripted(t, srv,
		ok(snmp.Idle), ok(snmp.Triggered), ok(snmp.Idle), ok(snmp.Idle),
	)

	if got := mp.accepted(); got != 1 {
		t.Errorf("healthy run must register exactly once, got %d — sequence %v", got, mp.sequence())
	}
	if got := script.lastPolled(t); got != 3 {
		t.Errorf("healthy run must keep polling instanceID 3, got %d", got)
	}
}
