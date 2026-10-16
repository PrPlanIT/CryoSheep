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

	"github.com/PrPlanIT/CryoSheep/src/core"
	"strings"
	"time"
)

const DefaultPort = "3493"

// Client reads one upsd. Reading needs no credentials; the one command it sends,
// the cut, needs User and Pass.
type Client struct {
	Addr    string // host:port
	UPS     string // UPS name as upsd knows it
	Timeout time.Duration
	User    string
	Pass    string
}

// Endpoints are several upsd instances serving the same UPS, read in order.
// The first answer wins, so one dead endpoint does not blind the gate.
type Endpoints []*Client

// Read returns the first endpoint's reading that succeeds.
func (e Endpoints) Read(ctx context.Context) (core.Reading, error) {
	err := fmt.Errorf("nut: no endpoint configured")
	for _, c := range e {
		var r core.Reading
		if r, err = c.Read(ctx); err == nil {
			return r, nil
		}
	}
	return core.Reading{}, err
}

// Runtime returns the first endpoint's battery.runtime that succeeds.
func (e Endpoints) Runtime(ctx context.Context) (time.Duration, error) {
	err := fmt.Errorf("nut: no endpoint configured")
	for _, c := range e {
		var d time.Duration
		if d, err = c.Runtime(ctx); err == nil {
			return d, nil
		}
	}
	return 0, err
}

// Cut asks the first endpoint that accepts it.
func (e Endpoints) Cut(ctx context.Context, after time.Duration) error {
	err := fmt.Errorf("nut: no endpoint configured")
	for _, c := range e {
		if err = c.Cut(ctx, after); err == nil {
			return nil
		}
	}
	return err
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

// Read takes one observation: status plus the values that show whether the
// battery is still draining.
//
// Missing variables are reported as Unknown rather than as an error. Drivers
// differ over what they expose — this one reports no low thresholds at all — and
// a gate that failed whenever a UPS was less forthcoming than expected would be
// useless on exactly the hardware it has to work with.
func (c *Client) Read(ctx context.Context) (core.Reading, error) {
	status, err := c.get(ctx, "ups.status")
	if err != nil {
		return core.Reading{}, err
	}
	r := core.Reading{Status: status, Charge: core.Unknown, Runtime: core.Unknown, InputV: core.Unknown}
	if v, err := c.get(ctx, "battery.charge"); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			r.Charge = f
		}
	}
	if v, err := c.get(ctx, "battery.runtime"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			r.Runtime = time.Duration(n) * time.Second
		}
	}
	if v, err := c.get(ctx, "input.voltage"); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			r.InputV = f
		}
	}
	return r, nil
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

// Cut sends shutdown.return with the delay in seconds: output off after it,
// back on once utility power is present. The driver has no usable default delay,
// so it is always given.
func (c *Client) Cut(ctx context.Context, after time.Duration) error {
	secs := int(after.Seconds())
	if secs <= 0 {
		return fmt.Errorf("nut: cut delay must be positive, got %s", after)
	}
	return c.InstCmd(ctx, "shutdown.return", strconv.Itoa(secs))
}

// InstCmd sends an instant command and waits for the driver's verdict.
//
// upsd's plain OK only means it queued the command. With tracking on it returns
// an id, and GET TRACKING reports PENDING until the driver answers SUCCESS or an
// ERR. A upsd without tracking falls back to the plain OK.
//
// NUT authentication is plaintext; the password is written to the socket only,
// never logged or placed in argv.
func (c *Client) InstCmd(ctx context.Context, cmd, param string) error {
	if c.User == "" || c.Pass == "" {
		return fmt.Errorf("nut: %s needs the MONITOR line's user and password", cmd)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
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
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	}
	r := bufio.NewReader(conn)

	ask := func(line string) (string, error) {
		if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
			return "", fmt.Errorf("nut write: %w", err)
		}
		reply, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("nut read: %w", err)
		}
		return strings.TrimSpace(reply), nil
	}
	must := func(line, what string) error {
		reply, err := ask(line)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(reply, "OK") {
			return fmt.Errorf("nut %s: %s", what, reply)
		}
		return nil
	}

	if err := must("USERNAME "+c.User, "USERNAME"); err != nil {
		return err
	}
	if err := must("PASSWORD "+c.Pass, "PASSWORD"); err != nil {
		return err
	}
	tracking := false
	if reply, err := ask("SET TRACKING ON"); err != nil {
		return err
	} else {
		tracking = strings.HasPrefix(reply, "OK")
	}

	line := fmt.Sprintf("INSTCMD %s %s", c.UPS, cmd)
	if param != "" {
		line += " " + param
	}
	reply, err := ask(line)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(reply, "OK") {
		return fmt.Errorf("nut %s: %s", cmd, reply)
	}
	id, ok := strings.CutPrefix(reply, "OK TRACKING ")
	if !tracking || !ok {
		return nil
	}
	for {
		st, err := ask("GET TRACKING " + strings.TrimSpace(id))
		if err != nil {
			return err
		}
		switch {
		case st == "SUCCESS":
			fmt.Fprint(conn, "LOGOUT\n")
			return nil
		case st != "PENDING":
			return fmt.Errorf("nut %s: %s", cmd, st)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("nut %s: still pending: %w", cmd, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
