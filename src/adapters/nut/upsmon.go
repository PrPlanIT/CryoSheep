package nut

import (
	"bufio"
	"os"
	"strings"
)

// UpsmonConf is where NUT already records how to reach the UPS on this host.
const UpsmonConf = "/etc/nut/upsmon.conf"

// Monitor is one MONITOR line from upsmon.conf:
//
//	MONITOR ups@localhost 1 monuser monpass master
//
// CryoSheep reads this rather than taking its own flags, because the UPS
// connection is deployment configuration that NUT already owns. A second place
// to write the same credentials is a second place for them to drift, to leak,
// and to be missed when they rotate.
type Monitor struct {
	Name string // UPS name as upsd knows it
	Addr string // host[:port]
	User string
	Pass string
	Prim bool // "master" (NUT 2.7) or "primary" (2.8+)
}

// ReadMonitor parses the first MONITOR line from the given upsmon.conf.
//
// Absence is not an error. A Kubernetes guest or a workstation has no NUT
// installed and no UPS to read; it is told to stop by something else and simply
// stops well. Callers distinguish the two by the boolean.
func ReadMonitor(path string) (Monitor, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Monitor{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "MONITOR") {
			continue
		}
		m := Monitor{Name: "ups"}
		if name, addr, ok := strings.Cut(fields[1], "@"); ok {
			m.Name, m.Addr = name, addr
		} else {
			m.Addr = fields[1]
		}
		// MONITOR <system> <powervalue> <user> <password> <type>
		if len(fields) >= 4 {
			m.User = fields[3]
		}
		if len(fields) >= 5 {
			m.Pass = fields[4]
		}
		if len(fields) >= 6 {
			t := strings.ToLower(fields[5])
			m.Prim = t == "master" || t == "primary"
		}
		return m, true
	}
	return Monitor{}, false
}
