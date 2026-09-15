// Package nut reads UPS state by speaking the NUT protocol directly.
//
// Implemented rather than shelled out to upsc for two reasons: a Proxmox host
// need not have the NUT client binaries installed, and this is the gate consulted
// before every reversible step — it must have a deadline it actually honours. A
// hung read here would stall a shutdown that is racing a battery.
//
// CryoSheep does not monitor NUT. upsmon owns the event stream and decides when
// the sequence starts; this only answers "is mains back?" at each gate.
package nut

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const DefaultPort = "3493"

type Client struct {
	Addr    string // host:port
	UPS     string // UPS name as upsd knows it
	Timeout time.Duration

	// Credentials for instant commands. Reading variables needs none; arming or
	// cancelling the UPS deadline does.
	User string
	Pass string
}

func New(addr, ups string) *Client {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, DefaultPort)
	}
	return &Client{Addr: addr, UPS: ups, Timeout: 3 * time.Second}
}

// Status returns the contents of ups.status, e.g. "OL" or "OB LB".
func (c *Client) Status(ctx context.Context) (string, error) {
	return c.get(ctx, "ups.status")
}

// get fetches one variable.
func (c *Client) get(ctx context.Context, name string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return "", fmt.Errorf("nut dial %s: %w", c.Addr, err)
	}
	defer conn.Close()

	// One deadline covers the whole exchange: a gate that cannot answer promptly
	// is as useless as one that answers wrongly.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if _, err := fmt.Fprintf(conn, "GET VAR %s %s\n", c.UPS, name); err != nil {
		return "", fmt.Errorf("nut write: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("nut read: %w", err)
	}
	fmt.Fprint(conn, "LOGOUT\n")

	return ParseVar(line)
}

// ParseVar reads upsd's reply to GET VAR.
//
//	VAR ups ups.status "OL"
//	ERR VAR-NOT-SUPPORTED
func ParseVar(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("nut: empty reply")
	}
	if strings.HasPrefix(line, "ERR") {
		return "", fmt.Errorf("nut: %s", line)
	}
	i := strings.Index(line, `"`)
	j := strings.LastIndex(line, `"`)
	if i < 0 || j <= i {
		return "", fmt.Errorf("nut: unparsable reply %q", line)
	}
	return line[i+1 : j], nil
}

// Flag reports whether a status string carries a flag, e.g. OL or LB. The value
// is a space-separated set, so membership is the only correct test — "OB LB"
// contains OB, and equality against "OB" would miss it.
func Flag(status, flag string) bool {
	for _, f := range strings.Fields(status) {
		if f == flag {
			return true
		}
	}
	return false
}

// Runtime returns battery.runtime, the value the trigger threshold is expressed
// in. Runtime rather than charge because it already folds load in: twenty
// percent at half load is minutes, at full load it is not.
func (c *Client) Runtime(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, "battery.runtime")
	if err != nil {
		return 0, err
	}
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("nut: battery.runtime %q: %w", v, err)
	}
	return time.Duration(secs) * time.Second, nil
}

// InstCmd sends an instant command.
//
// Unlike reading a variable this requires an authenticated user with instcmds
// permission, so User/Pass must be set. NUT authentication is plaintext, so the
// credential is only as protected as the path to upsd — worth remembering when
// deciding which networks may reach port 3493.
//
// The password is never logged, and never placed in argv: it is read from
// configuration and written straight to the socket.
func (c *Client) InstCmd(ctx context.Context, cmd, param string) error {
	if c.User == "" || c.Pass == "" {
		return fmt.Errorf("nut: %s needs credentials (instant commands are authenticated)", cmd)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("nut dial %s: %w", c.Addr, err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	r := bufio.NewReader(conn)

	say := func(line, what string) error {
		if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
			return fmt.Errorf("nut write %s: %w", what, err)
		}
		reply, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("nut read %s: %w", what, err)
		}
		reply = strings.TrimSpace(reply)
		if !strings.HasPrefix(reply, "OK") {
			return fmt.Errorf("nut %s: %s", what, reply)
		}
		return nil
	}

	if err := say("USERNAME "+c.User, "USERNAME"); err != nil {
		return err
	}
	// Only the outcome of this is ever surfaced, never the value.
	if err := say("PASSWORD "+c.Pass, "PASSWORD"); err != nil {
		return err
	}

	line := fmt.Sprintf("INSTCMD %s %s", c.UPS, cmd)
	if param != "" {
		line += " " + param
	}
	if err := say(line, cmd); err != nil {
		return err
	}
	fmt.Fprint(conn, "LOGOUT\n")
	return nil
}

// Arm sets the UPS to cut power after in. Implements core.Deadline.
func (c *Client) Arm(ctx context.Context, in time.Duration) error {
	secs := int(in.Seconds())
	if secs <= 0 {
		return fmt.Errorf("nut: deadline must be positive, got %s", in)
	}
	return c.InstCmd(ctx, "load.off.delay", strconv.Itoa(secs))
}

// Cancel stops a shutdown the UPS has already been told to perform.
// Implements core.Deadline.
func (c *Client) Cancel(ctx context.Context) error {
	return c.InstCmd(ctx, "shutdown.stop", "")
}
