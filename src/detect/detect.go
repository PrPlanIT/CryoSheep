// Package detect establishes what a host is from what is running on it.
//
// Roles are read from the init system rather than from an inventory on purpose.
// An inventory records intent and can be stale; a host that is no longer running
// ceph-osd should not have its shutdown sequence set noout, whatever a group_vars
// file still says. The cost of being wrong here is paid during a power failure,
// so the answer comes from the machine itself.
package detect

import (
	"context"
	"sort"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

// Roles returns every role whose unit is active, in a stable order.
//
// A unit that cannot be queried is treated as absent rather than as an error:
// a host that is partway through shutting down may already have stopped a unit,
// and refusing to produce a plan at that point is worse than producing one that
// skips a step whose subject is already gone.
func Roles(ctx context.Context, u core.Units) []core.Role {
	var found []core.Role
	for role, unit := range core.RoleUnits {
		active, err := u.IsActive(ctx, unit)
		if err != nil || !active {
			continue
		}
		found = append(found, role)
	}
	sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
	return found
}
