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
	m, ok := ReadMonitor(p)
	if !ok {
		t.Fatal("MONITOR line not found")
	}
	if m.Name != "ups" || m.Addr != "localhost" || m.User != "monuser" || m.Pass != "monpass" || !m.Prim {
		t.Fatalf("parsed wrong: %+v", m)
	}
}

// NUT 2.8 renamed master to primary; both must read as primary.
func TestPrimaryAndMasterBothMeanPrimary(t *testing.T) {
	for _, word := range []string{"master", "primary"} {
		m, _ := ReadMonitor(writeConf(t, "MONITOR ups@h 1 u p "+word+"\n"))
		if !m.Prim {
			t.Fatalf("%q did not read as primary", word)
		}
	}
	m, _ := ReadMonitor(writeConf(t, "MONITOR ups@h 1 u p slave\n"))
	if m.Prim {
		t.Fatal("slave read as primary")
	}
}

func TestAddressCarriesAPort(t *testing.T) {
	m, _ := ReadMonitor(writeConf(t, "MONITOR eaton@10.0.0.5:3493 1 u p slave\n"))
	if m.Name != "eaton" || m.Addr != "10.0.0.5:3493" {
		t.Fatalf("parsed wrong: %+v", m)
	}
}

// A guest has no NUT installed and no UPS to read. That is not an error — it is
// told to stop by something else and simply stops well.
func TestAbsentConfigIsNotAnError(t *testing.T) {
	if _, ok := ReadMonitor("/nonexistent/upsmon.conf"); ok {
		t.Fatal("reported a monitor from a file that does not exist")
	}
}
