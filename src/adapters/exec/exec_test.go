package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

func fake(out string, err error) *Runner {
	return &Runner{Run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(out), err
	}}
}

// Captured from a Proxmox host.
const qmListSample = `      VMID NAME                 STATUS     MEM(MB)    BOOTDISK(GB) PID
       102 dungeon-chest-002    running    16384             64.00 2841
       107 dungeon-map-002      running    8192              32.00 3012
       118 spare-template       stopped    4096              32.00 0
`

func TestParseQMList(t *testing.T) {
	g, err := ParseQMList(qmListSample)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 3 {
		t.Fatalf("got %d guests, want 3: %+v", len(g), g)
	}
	if g[0].ID != "102" || g[0].Name != "dungeon-chest-002" || !g[0].Running() {
		t.Fatalf("first guest wrong: %+v", g[0])
	}
	if g[2].Running() {
		t.Fatalf("stopped guest reported running: %+v", g[2])
	}
}

func TestParseQMListEmptyAndHeaderOnly(t *testing.T) {
	for _, in := range []string{"", "      VMID NAME STATUS MEM(MB) BOOTDISK(GB) PID\n"} {
		g, err := ParseQMList(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(g) != 0 {
			t.Fatalf("got %+v, want none", g)
		}
	}
}

// systemctl exits non-zero for an inactive unit. That is an answer, not a
// failure, and must not surface as an error.
func TestIsActiveTreatsNonZeroExitAsInactive(t *testing.T) {
	r := fake("inactive\n", errors.New("exit status 3"))
	active, err := r.IsActive(context.Background(), "ceph-osd.target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("inactive unit reported active")
	}
}

func TestIsActiveTrue(t *testing.T) {
	r := fake("active\n", nil)
	active, _ := r.IsActive(context.Background(), "pve-cluster.service")
	if !active {
		t.Fatal("active unit reported inactive")
	}
}

func TestShutdownUsesAcpiNotStop(t *testing.T) {
	var got []string
	r := &Runner{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}}
	if err := r.Shutdown(context.Background(), "102", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if got[0] != "qm" || got[1] != "shutdown" {
		t.Fatalf("called %v, want qm shutdown (qm stop bypasses the guest's own shutdown)", got)
	}
	joined := ""
	for _, a := range got {
		joined += a + " "
	}
	if want := "--timeout 30 "; !contains(joined, want) {
		t.Fatalf("called %q, want it to carry %q", joined, want)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestParseStartupOrder(t *testing.T) {
	const cfg = `boot: order=scsi0
cores: 4
memory: 8192
name: pfSense-z-1
startup: order=1,up=30,down=120
scsi0: local-lvm:vm-100-disk-0,size=32G
`
	if got := ParseStartupOrder(cfg); got != 1 {
		t.Fatalf("order = %d, want 1", got)
	}
}

// `boot: order=scsi0` is a different field entirely and must not be mistaken
// for a startup order — reading it would invent a priority nobody set.
func TestBootOrderIsNotStartupOrder(t *testing.T) {
	const cfg = `boot: order=scsi0;net0
cores: 4
name: scratch
`
	if got := ParseStartupOrder(cfg); got != core.OrderUnset {
		t.Fatalf("order = %d, want unset — `boot:` is not `startup:`", got)
	}
}

func TestStartupWithoutOrderIsUnset(t *testing.T) {
	if got := ParseStartupOrder("startup: up=30,down=120\nname: x\n"); got != core.OrderUnset {
		t.Fatalf("order = %d, want unset", got)
	}
}

func TestMissingStartupIsUnset(t *testing.T) {
	if got := ParseStartupOrder("cores: 2\nname: x\n"); got != core.OrderUnset {
		t.Fatalf("order = %d, want unset", got)
	}
}

// A guest from qm list carries no order until its config has been read.
func TestQMListLeavesOrderUnset(t *testing.T) {
	g, _ := ParseQMList(qmListSample)
	for _, x := range g {
		if x.Order != core.OrderUnset {
			t.Fatalf("guest %s came out of qm list with order %d", x.ID, x.Order)
		}
	}
}
