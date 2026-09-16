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

func TestParseStartupDown(t *testing.T) {
	const cfg = "name: pfSense\nstartup: order=1,up=30,down=180\n"
	if got := ParseStartupDown(cfg); got != 180*time.Second {
		t.Fatalf("down = %v, want 180s", got)
	}
	if got := ParseStartupOrder(cfg); got != 1 {
		t.Fatalf("order = %d, want 1", got)
	}
}

func TestStartupDownAbsentIsZero(t *testing.T) {
	if got := ParseStartupDown("startup: order=1\nname: x\n"); got != 0 {
		t.Fatalf("down = %v, want 0 so the default applies", got)
	}
	if got := ParseStartupDown("name: x\n"); got != 0 {
		t.Fatalf("down = %v, want 0", got)
	}
}

// `boot: order=scsi0` must not be mistaken for a startup field in either reader.
func TestBootLineIgnoredByBothReaders(t *testing.T) {
	const cfg = "boot: order=scsi0;net0\nname: x\n"
	if got := ParseStartupOrder(cfg); got != core.OrderUnset {
		t.Fatalf("order = %d, want unset", got)
	}
	if got := ParseStartupDown(cfg); got != 0 {
		t.Fatalf("down = %v, want 0", got)
	}
}

// Captured from a k8s node with Ceph CSI volumes attached.
const findmntSample = `/                                                          ext4
/boot                                                      ext4
/run/lock                                                  tmpfs
/var/lib/kubelet/pods/9f2/volumes/kubernetes.io~csi/pvc-1/mount  ext4
/var/lib/kubelet/plugins/kubernetes.io/csi/cephfs/abc/globalmount ceph
/mnt/media                                                 cifs
/var/lib/docker/overlay2/abc/merged                        overlay
`

// Unmounting the wrong thing on a live node is worse than unmounting nothing, so
// the selection is narrow: Ceph, and the kubelet trees CSI drivers build under.
func TestCSIMountsSelectsOnlyCephAndKubelet(t *testing.T) {
	got := CSIMounts(findmntSample)
	want := map[string]bool{
		"/var/lib/kubelet/pods/9f2/volumes/kubernetes.io~csi/pvc-1/mount":   true,
		"/var/lib/kubelet/plugins/kubernetes.io/csi/cephfs/abc/globalmount": true,
	}
	if len(got) != len(want) {
		t.Fatalf("selected %v, want exactly %d kubelet/ceph mounts", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("selected %q, which is not a Ceph or CSI mount", g)
		}
	}
}

// The root filesystem, /boot and a user's CIFS share must never be selected.
func TestCSIMountsLeavesSystemMountsAlone(t *testing.T) {
	for _, g := range CSIMounts(findmntSample) {
		switch g {
		case "/", "/boot", "/mnt/media", "/run/lock":
			t.Fatalf("selected %q — unmounting this would break the node, not tidy it", g)
		}
	}
}

func TestCSIMountsHandlesEmptyOutput(t *testing.T) {
	if got := CSIMounts(""); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

const podsJSON = `{"items":[
 {"metadata":{"name":"pg-1","namespace":"db","labels":{"cnpg.io/instanceRole":"primary"},
   "ownerReferences":[{"kind":"Cluster","name":"pg"}]}},
 {"metadata":{"name":"pg-2","namespace":"db","labels":{"cnpg.io/instanceRole":"replica"},
   "ownerReferences":[{"kind":"Cluster","name":"pg"}]}},
 {"metadata":{"name":"loki-0","namespace":"obs","labels":{},
   "ownerReferences":[{"kind":"StatefulSet","name":"loki"}]}},
 {"metadata":{"name":"nginx-abc","namespace":"web","labels":{},
   "ownerReferences":[{"kind":"ReplicaSet","name":"nginx"}]}}
]}`

// The record exists so a revival knows who held authority. A primary must be
// distinguishable from a replica in it.
func TestParseStatefulPodsKeepsRoles(t *testing.T) {
	got, err := ParseStatefulPods([]byte(podsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("recorded %d pods, want 3 stateful ones: %+v", len(got), got)
	}
	// The label is kept whole, so the record carries the operator's own wording.
	var primary string
	for _, p := range got {
		if p.Role == "cnpg.io/instanceRole=primary" {
			primary = p.Namespace + "/" + p.Name
		}
	}
	if primary != "db/pg-1" {
		t.Fatalf("primary recorded as %q, want db/pg-1; roles seen: %+v", primary, got)
	}
}

// A stateless pod reschedules and its identity neither survives nor matters.
func TestParseStatefulPodsSkipsStateless(t *testing.T) {
	got, _ := ParseStatefulPods([]byte(podsJSON))
	for _, p := range got {
		if p.Name == "nginx-abc" {
			t.Fatal("a ReplicaSet pod was recorded")
		}
	}
}

func TestParseStatefulPodsEmpty(t *testing.T) {
	got, err := ParseStatefulPods([]byte(`{"items":[]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// The record has to be useful for a cluster running something nobody here has
// heard of, so roles are found by the words operators use — not by a list of
// the operators someone remembered.
func TestRoleOfFindsUnknownOperators(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		labels     map[string]string
	}{
		{"cnpg", "cnpg.io/instanceRole=primary", map[string]string{"cnpg.io/instanceRole": "primary"}},
		{"plain role", "role=master", map[string]string{"role": "master"}},
		{"vendor nobody knows", "acme.example/db-leader=true", map[string]string{"acme.example/db-leader": "true"}},
		{"value carries it", "state=standby", map[string]string{"state": "standby"}},
		{"none", "", map[string]string{"app": "web", "tier": "frontend"}},
	} {
		if got := roleOf(tc.labels); got != tc.want {
			t.Fatalf("%s: roleOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Two runs of the same cluster must record the same label, or the evidence
// contradicts itself between hosts.
func TestRoleOfIsDeterministic(t *testing.T) {
	labels := map[string]string{
		"zzz/role": "replica", "aaa/leader": "false", "mmm/primary": "no",
	}
	first := roleOf(labels)
	for i := 0; i < 50; i++ {
		if got := roleOf(labels); got != first {
			t.Fatalf("roleOf varied between calls: %q then %q", first, got)
		}
	}
}

// The operator's own vocabulary is preserved: a normalised guess is the kind of
// confidently-wrong that hurts during a recovery.
func TestRoleKeepsTheOperatorsWording(t *testing.T) {
	got := roleOf(map[string]string{"cnpg.io/instanceRole": "primary"})
	if got != "cnpg.io/instanceRole=primary" {
		t.Fatalf("role = %q, want the label verbatim", got)
	}
}
