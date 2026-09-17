# Deploying CryoSheep

Two units. One tears the machine down however it is being stopped; the other puts
it back on the way up.

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
| `upsmon` SHUTDOWNCMD | full sequence, including poweroff |
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

The one number worth setting is the UPS deadline, commented out in the unit:

```ini
Environment=CRYOSHEEP_UPS_DEADLINE=5m30s
```

Take it from `cryosheep calibrate`. It sits between two failure modes — too short
cuts power mid-flush and produces exactly the unclean stop everything else exists
to prevent; too long and the battery is flat before it fires. Unset means no
hardware backstop, which is the safe default until you have measured.

## Wiring upsmon

Power events reach CryoSheep through NUT, on the hosts that can read the UPS:

```
NOTIFYCMD   /usr/local/bin/cryosheep-notify
SHUTDOWNCMD "/usr/local/bin/cryosheep sleep"

NOTIFYFLAG ONBATT   SYSLOG+EXEC
NOTIFYFLAG ONLINE   SYSLOG+EXEC
NOTIFYFLAG LOWBATT  SYSLOG+EXEC
```

where `cryosheep-notify` dispatches on `NOTIFYTYPE`:

```sh
#!/bin/sh
case "$NOTIFYTYPE" in
  ONBATT)  exec /usr/local/bin/cryosheep conserve ;;
  ONLINE)  exec /usr/local/bin/cryosheep cancel ;;
esac
```

Guests need none of this. They have no route to `upsd` and no decision to make —
the hypervisor stops them, and if mains return it starts them again.

## Reading what happened

```
journalctl -t cryosheep -b -1      # the previous boot, on the machine itself
{unit="cryosheep-stasis.service"}  # in Loki, across the fleet
```

A run that was cut leaves a `step.start` with no `step.end`. That absence is the
evidence, and `cryosheep wake` reports it on the way back up.
