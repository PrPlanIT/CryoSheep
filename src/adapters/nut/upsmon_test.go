package nut

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upsmon.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadsTheMonitorLine(t *testing.T) {
	p := writeConf(t, "# comment\nMINSUPPLIES 1\nMONITOR ups@localhost 1 monuser monpass master\n")
	ms, ok := ReadMonitors(p)
	if !ok || len(ms) != 1 {
		t.Fatalf("MONITOR lines = %+v, want one", ms)
	}
	if m := ms[0]; m.Name != "ups" || m.Addr != "localhost" || !m.Prim || m.User != "monuser" || m.Pass != "monpass" {
		t.Fatalf("parsed wrong: %+v", m)
	}
}

// Two upsd endpoints for one UPS are two MONITOR lines; both are read, in order.
func TestReadsEveryMonitorLine(t *testing.T) {
	p := writeConf(t, "MONITOR eaton@nut-0:3493 1 u p secondary\nMONITOR eaton@nut-1:3493 1 u p secondary\n")
	ms, _ := ReadMonitors(p)
	if len(ms) != 2 || ms[0].Addr != "nut-0:3493" || ms[1].Addr != "nut-1:3493" {
		t.Fatalf("parsed wrong: %+v", ms)
	}
}

// NUT 2.8 renamed master to primary; both must read as primary.
func TestPrimaryAndMasterBothMeanPrimary(t *testing.T) {
	for _, word := range []string{"master", "primary"} {
		ms, _ := ReadMonitors(writeConf(t, "MONITOR ups@h 1 u p "+word+"\n"))
		if !ms[0].Prim {
			t.Fatalf("%q did not read as primary", word)
		}
	}
	ms, _ := ReadMonitors(writeConf(t, "MONITOR ups@h 1 u p secondary\n"))
	if ms[0].Prim {
		t.Fatal("secondary read as primary")
	}
}

func TestAddressCarriesAPort(t *testing.T) {
	ms, _ := ReadMonitors(writeConf(t, "MONITOR eaton@10.0.0.5:3493 1 u p secondary\n"))
	if ms[0].Name != "eaton" || ms[0].Addr != "10.0.0.5:3493" {
		t.Fatalf("parsed wrong: %+v", ms[0])
	}
}

// A guest has no NUT installed and no UPS to read. That is not an error — it is
// told to stop by something else and simply stops well.
func TestAbsentConfigIsNotAnError(t *testing.T) {
	if _, ok := ReadMonitors("/nonexistent/upsmon.conf"); ok {
		t.Fatal("reported a monitor from a file that does not exist")
	}
}
