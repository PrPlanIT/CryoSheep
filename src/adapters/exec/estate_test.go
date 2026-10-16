package exec

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const clusterResources = `[
 {"id":"qemu/206","vmid":206,"name":"Dungeon-Chest-002","node":"Bamboo","status":"running","type":"qemu"},
 {"id":"qemu/200","vmid":200,"name":"shield-regenerator-pfSense","node":"Bamboo","status":"running","type":"qemu"},
 {"id":"qemu/299","vmid":299,"name":"shield-regenerator-OPNsense","node":"Bamboo","status":"stopped","type":"qemu"},
 {"id":"lxc/900","vmid":900,"name":"ct","node":"Cosmos","status":"running","type":"lxc"}
]`

func TestParseClusterResourcesKeepsOnlyVMs(t *testing.T) {
	gs, err := ParseClusterResources([]byte(clusterResources))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 3 {
		t.Fatalf("got %d guests, want the three VMs: %+v", len(gs), gs)
	}
	if gs[0].ID != "206" || gs[0].Node != "Bamboo" || !gs[0].Running() {
		t.Fatalf("parsed wrong: %+v", gs[0])
	}
}

// Silence is not an empty cluster. Reading it as "nothing running" would
// release a hold early.
func TestAnEmptyListingIsAnError(t *testing.T) {
	if _, err := ParseClusterResources([]byte(`[]`)); err == nil {
		t.Fatal("an empty listing read as an empty cluster")
	}
	if _, err := ParseClusterResources([]byte(`not json`)); err == nil {
		t.Fatal("garbage read as a listing")
	}
}

func TestRunningReadsEachOrderFromTheClusterFilesystem(t *testing.T) {
	confs := map[string]string{
		filepath.Join(PVEConfDir, "Bamboo", "qemu-server", "206.conf"): "name: chest\nstartup: order=4,down=240\n\n[pre-upgrade]\nstartup: order=9\n",
		filepath.Join(PVEConfDir, "Bamboo", "qemu-server", "200.conf"): "startup: order=1,down=120\n",
	}
	r := &Runner{
		Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(clusterResources), nil },
		ReadFile: func(name string) ([]byte, error) {
			if c, ok := confs[name]; ok {
				return []byte(c), nil
			}
			return nil, errors.New("no such file")
		},
	}
	gs, err := r.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("got %+v, want the two running VMs", gs)
	}
	if gs[0].Order != 4 || gs[1].Order != 1 {
		t.Fatalf("orders = %d, %d; want 4 and 1 from the live section, not the snapshot", gs[0].Order, gs[1].Order)
	}
}

func TestParseTags(t *testing.T) {
	got := ParseTags("name: moor\ntags: docker;shed\n[snap]\ntags: old\n")
	if len(got) != 2 || got[0] != "docker" || got[1] != "shed" {
		t.Fatalf("ParseTags = %v, want [docker shed]", got)
	}
	if ParseTags("name: x\n") != nil {
		t.Fatal("a guest with no tags has tags")
	}
}
