package detect

import (
	"context"
	"errors"
	"testing"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

type fakeUnits struct {
	active map[string]bool
	fail   map[string]bool
}

func (f fakeUnits) IsActive(_ context.Context, unit string) (bool, error) {
	if f.fail[unit] {
		return false, errors.New("dbus unavailable")
	}
	return f.active[unit], nil
}

func has(roles []core.Role, want core.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func TestDetectsOnlyActiveRoles(t *testing.T) {
	u := fakeUnits{active: map[string]bool{
		"pve-cluster.service": true,
		"ceph-osd.target":     true,
		"kubelet.service":     false,
	}}
	got := Roles(context.Background(), u)
	if len(got) != 2 || !has(got, core.RoleProxmox) || !has(got, core.RoleCephOSD) {
		t.Fatalf("roles = %v, want proxmox and ceph-osd only", got)
	}
}

func TestBareHostHasNoRoles(t *testing.T) {
	if got := Roles(context.Background(), fakeUnits{active: map[string]bool{}}); len(got) != 0 {
		t.Fatalf("roles = %v, want none", got)
	}
}

// A unit that cannot be queried is absent, not fatal — the host may already be
// partway through stopping it.
func TestUnqueryableUnitIsTreatedAsAbsent(t *testing.T) {
	u := fakeUnits{
		active: map[string]bool{"pve-cluster.service": true},
		fail:   map[string]bool{"ceph-osd.target": true},
	}
	got := Roles(context.Background(), u)
	if len(got) != 1 || !has(got, core.RoleProxmox) {
		t.Fatalf("roles = %v, want proxmox only", got)
	}
}

func TestOrderIsStable(t *testing.T) {
	u := fakeUnits{active: map[string]bool{
		"pve-cluster.service": true,
		"ceph-osd.target":     true,
		"kubelet.service":     true,
	}}
	a := Roles(context.Background(), u)
	for i := 0; i < 20; i++ {
		b := Roles(context.Background(), u)
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("unstable order: %v then %v", a, b)
			}
		}
	}
}
