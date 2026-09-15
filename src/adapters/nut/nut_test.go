package nut

import (
	"bufio"
	"context"
	"net"
	"strings"
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
func fakeUpsd(t *testing.T, replies map[string]string) (addr string, got *[]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	lines := &[]string{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			*lines = append(*lines, line)
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
	}()
	return ln.Addr().String(), lines
}

func TestArmSendsLoadOffDelayWithSeconds(t *testing.T) {
	addr, got := fakeUpsd(t, nil)
	c := New(addr, "ups")
	c.User, c.Pass = "cryosheep", "secret"

	if err := c.Arm(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	// LOGOUT is courtesy sent after the command is already acknowledged, and its
	// delivery races the close — the meaningful exchange is what precedes it.
	want := []string{"USERNAME cryosheep", "PASSWORD secret", "INSTCMD ups load.off.delay 90"}
	if len(*got) < len(want) {
		t.Fatalf("exchange = %v, want it to begin %v", *got, want)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Fatalf("exchange = %v, want it to begin %v", *got, want)
		}
	}
}

func TestCancelSendsShutdownStop(t *testing.T) {
	addr, got := fakeUpsd(t, nil)
	c := New(addr, "ups")
	c.User, c.Pass = "u", "p"
	if err := c.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range *got {
		if l == "INSTCMD ups shutdown.stop" {
			found = true
		}
	}
	if !found {
		t.Fatalf("exchange = %v, want an INSTCMD shutdown.stop", *got)
	}
}

// A rejected credential must surface as an error, not be mistaken for success —
// silently failing to arm the backstop would remove the one guarantee that does
// not depend on software completing.
func TestRejectedCredentialIsAnError(t *testing.T) {
	addr, _ := fakeUpsd(t, map[string]string{"PASSWORD": "ERR ACCESS-DENIED"})
	c := New(addr, "ups")
	c.User, c.Pass = "u", "wrong"
	err := c.Arm(context.Background(), 60*time.Second)
	if err == nil {
		t.Fatal("expected an error on a rejected password")
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestInstCmdRefusesWithoutCredentials(t *testing.T) {
	c := New("127.0.0.1:1", "ups")
	if err := c.Cancel(context.Background()); err == nil {
		t.Fatal("expected a refusal without credentials")
	}
}

func TestArmRejectsNonPositiveDeadline(t *testing.T) {
	c := New("127.0.0.1:1", "ups")
	c.User, c.Pass = "u", "p"
	if err := c.Arm(context.Background(), 0); err == nil {
		t.Fatal("expected an error arming a zero deadline")
	}
}
