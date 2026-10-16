# Deploying CryoSheep

Two units. One tears the machine down however it is being stopped; the other puts
it back on the way up. A third, `cryosheep@.service`, is started by NUT on a UPS
event and is never enabled.

```
install -m0755 cryosheep /usr/local/bin/cryosheep
install -m0644 deploy/systemd/*.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now cryosheep-stasis.service cryosheep-wake.service
```

`cryosheep-stasis` does nothing at boot; its `ExecStop` is the whole point.
Enabling it is what arms the teardown.

## What now triggers it

| how the machine stops | what happens |
|---|---|
| `systemctl poweroff` | teardown, then systemd halts |
| `systemctl reboot` | teardown, then systemd reboots — **no poweroff** |
| `qm shutdown` from the hypervisor | teardown, then systemd halts |
| `ansible.builtin.reboot` in the patch cycle | teardown, then reboot |
| `upssched` commit timer, or `upsmon` SHUTDOWNCMD at `runtime.low` | full sequence, UPS cut, poweroff |
| `systemctl stop cryosheep-stasis` | **nothing** — see below |

That last row matters. `ExecStop` fires whenever the unit stops, so stopping it by
hand would otherwise cordon a healthy node and release its mounts. CryoSheep asks
systemd whether a shutdown is actually in progress and declines when it is not.

## Verify before trusting it

```
cryosheep plan                          # what a requested stop would do
INVOCATION_ID=x cryosheep plan          # what the unit will do — no host.poweroff
systemctl stop cryosheep-stasis         # must report "the machine is not shutting down"
systemctl start cryosheep-stasis
```

## Ordering, and why it is written that way

systemd stops units in the reverse of the order it starts them. The unit is
ordered `After=` the network, the kubelet, and the PVE services precisely so it
runs **before** them on the way down — while the API server is reachable, the CSI
mounts still exist, and the network is still up.

Units that are not installed on a given host are ignored, so the same file works
unchanged on a Kubernetes node and a Proxmox hypervisor.

On a Proxmox host this deliberately runs before `pve-guests.service`. Both stop
guests in reverse `startup: order=`; doing it here first means the run is recorded
and, during a power event, can be abandoned if mains return. `pve-guests` then
finds the guests already stopped and has nothing to do.

## Configuration

Almost none. The UPS connection is read from `/etc/nut/upsmon.conf`, the
per-guest budget from Proxmox `startup: down=`, and why the machine is stopping
from the environment of whatever invoked it.

Four settings shape a power event:

```ini
Environment=CRYOSHEEP_GUEST_CONCURRENCY=-1
Environment=CRYOSHEEP_ORDER_GROUPS=4-99,1-3@4m10s
Environment=CRYOSHEEP_HOLD_UNTIL=5m30s
Environment=CRYOSHEEP_UPS_CUT=45s
```

`GUEST_CONCURRENCY` is how many guests may stop at once — `1` one at a time,
`-1` every guest in an order tier together, `n` a bounded roll. Unset means one
at a time, because concurrency coarsens when a sequence can be abandoned: guests
issued together cannot be abandoned between them, so mains returning mid-wave
stops all of them and then restarts all of them. Guests that declare a Proxmox
`startup: order=` are never merged across tiers whatever this says.

`ORDER_GROUPS` stops workloads and leaves (orders 4-99) together, then the core
(storage, DNS, routers, 1-3). `@4m10s` holds the core until 4m10s after the run
began — the Kubernetes `down=` of 240s plus the force-stop — or until no guest in
4-99 is running on any node, whichever is first. The routers carry every
Kubernetes PVC, so they must outlive kubelet on every node, and `order=` is per
host. The estate is read from `/etc/pve`, which travels over corosync rather than
through the routers; a stale reading only lengthens the wait.

`HOLD_UNTIL` makes the first step that cannot be undone wait until that long
after the run began. It is for shared storage: with Ceph OSDs on the machines
being stopped, a host that halts the moment it is ready takes its OSDs while
another host's guests are still writing, and below `min_size` the stragglers
freeze mid-sync. Set it on every host to the same value and they commit
together; it releases early once no guest is running anywhere. Unset means halt
when ready, which is right for a host whose storage is its own.

`UPS_CUT` is the delay sent with `shutdown.return` as the last step before the
halt: the output goes off that long later and comes back once utility power is
present. Every host sends it at the barrier, so the cut lands after the estate is
down whichever command the UPS keeps. It is also what brings the hosts back: a
host that halted with mains present stays off until the output cycles. A failed
cut does not stop the halt; the UPS's own outage shutdown is the backstop. The
host needs a route to the UPS that does not pass through anything it has just
stopped.

Groups, holds and the cut apply only when the UPS reports the power failure. A
reboot or maintenance stop keeps every tier its own wave, waits on nobody and
sends nothing.

These are ceilings. The typical run is the Kubernetes wave, the core (~20s) and
the halt (~20s), with holds released early. The worst case counts every hold at
its ceiling and every guest at its `down=`.

`cryosheep plan --power-loss` prints the power-event plan on an ordinary day —
steps that run together are marked, every hold says what releases it, and the
worst case is set against the battery.

## Stopping a machine CryoSheep does not run on

Running on the host is preferable and needs no credential at all: `qm` and
`systemctl` are simply there. Use this only for what cannot take an agent — an
appliance, a NAS, a hypervisor you do not control.

```ini
Environment=CRYOSHEEP_PROXMOX_URL=https://eggplant.example:8006
Environment=CRYOSHEEP_PROXMOX_TOKEN_ID=cryosheep@pve!shutdown
Environment=CRYOSHEEP_PROXMOX_TOKEN_SECRET=...
Environment=CRYOSHEEP_PROXMOX_NODE=eggplant
```

Give the token `Sys.PowerMgmt` and nothing else. It can then stop guests and halt
the node, and can do nothing at all besides — which is the entire reason to
prefer it over an SSH key that could do anything, on every host, forever.

All four are required. A partial configuration warns and acts on the local
machine rather than guessing, because stopping the wrong host is worse than
stopping none. When a remote target is set, the Ceph and kubernetes steps are
dropped: they act on this machine, and this machine is not the one being stopped.

## Wiring NUT

Each host runs its own driver and `upsd` against the UPS, and its `upsmon` is a
secondary of that local `upsd`. Nothing between the host and the UPS is stopped
by the sequence, so the host can still send the cut at the end. CryoSheep reads
the same `MONITOR` line, and uses its user for the cut.

`ups.conf`:

```
[eaton]
  driver = snmp-ups
  port = <ups address>
  override.battery.runtime.low = 180
```

`upsd.conf` listens on `127.0.0.1` only. `upsd.users`:

```
[cryosheep]
  password = <password>
  instcmds = shutdown.return
  upsmon secondary
```

`upsmon.conf`:

```
MONITOR eaton@localhost 1 cryosheep <password> secondary
MINSUPPLIES 1
DEADTIME    300

SHUTDOWNCMD "/bin/systemctl start --no-block cryosheep@sleep.service"
RUN_AS_USER root
NOTIFYCMD   /usr/sbin/upssched

NOTIFYFLAG ONBATT  SYSLOG+EXEC
NOTIFYFLAG ONLINE  SYSLOG+EXEC
```

A secondary, so `upsmon` never writes `POWERDOWNFLAG` and NUT's own
end-of-shutdown command is never sent. For this driver that sequence falls back
to `load.off.delay`, which cuts the output and does not restore it.

Every host reads the same UPS with the same `override.battery.runtime.low`, so
they reach the same verdict. `runtime.low` is the floor, for a battery weaker than
the timer assumes; the timer below is what normally commits. `DEADTIME` is long
because losing sight of the UPS must not decide anything.

The hooks start `cryosheep@<verb>.service` rather than running CryoSheep
themselves. `upsmon` exits once it has handed off SHUTDOWNCMD, and systemd then
kills what is left in `nut-monitor`'s cgroup, which would include a sequence
started there. The unit also runs one instance: the timer and the floor both
start `@sleep`, and the second start does nothing. `RUN_AS_USER root` is what lets
`upssched` call `systemctl`; the verbs need root for `qm` anyway.

Give `nut-monitor.service` `Restart=always`: `upsmon` exits after SHUTDOWNCMD,
and the host is unmonitored after the first recovered outage otherwise.

`upssched` sheds after a linger and commits at a fixed time on battery:

```
CMDSCRIPT /usr/local/bin/cryosheep-sched
PIPEFN    /run/nut/upssched/upssched.pipe
LOCKFN    /run/nut/upssched/upssched.lock
AT ONBATT * START-TIMER shed 60
AT ONBATT * START-TIMER commit 90
AT ONLINE * CANCEL-TIMER shed
AT ONLINE * CANCEL-TIMER commit
AT ONLINE * EXECUTE online
```

```sh
#!/bin/sh
case "$1" in
  shed)   exec systemctl start --no-block cryosheep@conserve.service ;;
  commit) exec systemctl start --no-block cryosheep@sleep.service ;;
  online) exec systemctl start --no-block cryosheep@cancel.service ;;
esac
```

`conserve` stops only guests tagged `shed` in Proxmox. `sleep` runs one sequence
per host, so the timer and the floor cannot start two, and does nothing if mains
are already back. `cancel` undoes the latest run and the runs that led up to it
in the same event, and records that it did, so a second ONLINE finds nothing to
undo. `conserve`, `cancel` and `wake` run one at a time on a host.

Guests need none of this. They have no route to `upsd` and no decision to make —
the hypervisor stops them, and if mains return it starts them again.

## Reading what happened

```
journalctl -t cryosheep -b -1      # the previous boot, on the machine itself
{unit="cryosheep-stasis.service"}  # in Loki, across the fleet
```

A run that was cut leaves a `step.start` with no `step.end`. That absence is the
evidence, and `cryosheep wake` reports it on the way back up.
