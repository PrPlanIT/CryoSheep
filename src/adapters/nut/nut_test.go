package nut

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseVar(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`VAR ups ups.status "OL"`, "OL"},
		{`VAR ups ups.status "OB LB"`, "OB LB"},
		{"VAR ups ups.status \"OL CHRG\"\n", "OL CHRG"},
	} {
		got, err := ParseVar(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseVarErrors(t *testing.T) {
	for _, in := range []string{"", "ERR VAR-NOT-SUPPORTED", "garbage without quotes"} {
		if _, err := ParseVar(in); err == nil {
			t.Fatalf("%q: expected an error", in)
		}
	}
}

// "OB LB" must register as on-battery. Equality against "OB" is the bug this
// guards: by the time LB is set the sequence is committed, and a gate that
// stopped recognising OB would read a dying UPS as healthy.
func TestFlagMembershipNotEquality(t *testing.T) {
	if !Flag("OB LB", "OB") {
		t.Fatal(`Flag("OB LB","OB") = false`)
	}
	if !Flag("OB LB", "LB") {
		t.Fatal(`Flag("OB LB","LB") = false`)
	}
	if Flag("OB LB", "OL") {
		t.Fatal(`Flag("OB LB","OL") = true`)
	}
	if Flag("OL", "OL") == false {
		t.Fatal(`Flag("OL","OL") = false`)
	}
}

func TestStatusAgainstFakeUpsd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		conn.Write([]byte("VAR ups ups.status \"OB LB\"\n"))
	}()

	c := New(ln.Addr().String(), "ups")
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "OB LB" {
		t.Fatalf("status = %q, want %q", got, "OB LB")
	}
}

// A upsd that accepts the connection and never answers must not stall the gate.
func TestStatusHonoursDeadline(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(2 * time.Second) // never replies in time
	}()

	c := New(ln.Addr().String(), "ups")
	c.Timeout = 150 * time.Millisecond

	start := time.Now()
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked for %v; the gate must not outlive its deadline", elapsed)
	}
}

func TestNewDefaultsPort(t *testing.T) {
	if c := New("upsd.example", "ups"); c.Addr != "upsd.example:3493" {
		t.Fatalf("Addr = %q, want default port applied", c.Addr)
	}
	if c := New("upsd.example:9999", "ups"); c.Addr != "upsd.example:9999" {
		t.Fatalf("Addr = %q, want explicit port preserved", c.Addr)
	}
}

// fakeUpsd speaks just enough of the protocol to verify the exchange, and
// records what it was told.
// recorded is the lines the fake upsd saw. It is behind a mutex because the
// server runs on its own goroutine and the test reads it from another.
type recorded struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorded) add(l string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, l)
}

func (r *recorded) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func fakeUpsd(t *testing.T, replies map[string]string) (addr string, got *recorded) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	lines := &recorded{}
	serve := func(conn net.Conn) {
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			lines.add(line)
			if line == "LOGOUT" {
				return
			}
			verb := strings.Fields(line)[0]
			if reply, ok := replies[verb]; ok {
				conn.Write([]byte(reply + "\n"))
			} else {
				conn.Write([]byte("OK\n"))
			}
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return ln.Addr().String(), lines
}

// Two upsd endpoints carry the same UPS. A read answered by either is a read; a
// dead first endpoint must not blind the gate.
func TestEndpointsFallThroughToALiveOne(t *testing.T) {
	addr, _ := fakeUpsd(t, map[string]string{"GET": `VAR ups ups.status "OB"`})
	e := Endpoints{New("127.0.0.1:1", "ups"), New(addr, "ups")}
	r, err := e.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "OB" {
		t.Fatalf("Status = %q, want OB from the live endpoint", r.Status)
	}
}

func TestEndpointsAllDeadIsAnError(t *testing.T) {
	e := Endpoints{New("127.0.0.1:1", "ups"), New("127.0.0.1:2", "ups")}
	if _, err := e.Read(context.Background()); err == nil {
		t.Fatal("expected an error when no endpoint answers")
	}
}

func TestCutWaitsForTheDriversVerdict(t *testing.T) {
	addr, got := fakeUpsd(t, map[string]string{"INSTCMD": "OK TRACKING 1f2e", "GET": "SUCCESS"})
	c := New(addr, "eaton")
	c.User, c.Pass = "cryosheep", "pw"
	if err := c.Cut(context.Background(), 45*time.Second); err != nil {
		t.Fatal(err)
	}
	want := []string{"USERNAME cryosheep", "PASSWORD pw", "SET TRACKING ON", "INSTCMD eaton shutdown.return 45", "GET TRACKING 1f2e"}
	lines := got.all()
	for i, w := range want {
		if i >= len(lines) || lines[i] != w {
			t.Fatalf("conversation = %q, want it to start %q", lines, want)
		}
	}
}

func TestCutReportsTheDriversRefusal(t *testing.T) {
	addr, _ := fakeUpsd(t, map[string]string{"INSTCMD": "OK TRACKING 1f2e", "GET": "ERR CMD-NOT-SUPPORTED"})
	c := New(addr, "eaton")
	c.User, c.Pass = "cryosheep", "pw"
	if err := c.Cut(context.Background(), 45*time.Second); err == nil {
		t.Fatal("a refused command reported success")
	}
}

func TestCutNeedsCredentials(t *testing.T) {
	if err := New("127.0.0.1:1", "eaton").Cut(context.Background(), time.Minute); err == nil {
		t.Fatal("sent a command without credentials")
	}
}
