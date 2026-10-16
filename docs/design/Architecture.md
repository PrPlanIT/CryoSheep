# Architecture

What CryoSheep is, where it runs, what triggers it, and what it must never do.

This is the plan of record. It supersedes scattered decisions made in conversation.
Invariants live in `Invariants_Dump.md`; operator-facing surfaces live in
`Presentation_Layer.md`.

---

## 1. What CryoSheep is

**A shutdown handler for a machine that has complicated things on it.** Nothing else.

Three questions, three owners. CryoSheep owns exactly one:

| question | owner |
|---|---|
| Are the mains out? | The UPS, via NUT |
| Should *this* machine stop now? | `upsmon` on that machine, or systemd, or an operator |
| How does *this* machine stop cleanly? | **CryoSheep** |

It is deliberately local-only: it never reaches across a host boundary. A sequencer
that shuts down its own host cannot finish a loop over other hosts, and a tool that
can stop machines it does not live on is a tool that can stop the wrong one.

### What it is not

- **Not a NUT orchestrator.** It does not elect a master, relay UPS state, or decide
  who monitors what. NUT already answers that.
- **Not a UPS exporter.** The Eaton exporter already collects telemetry.
- **Not a monitoring system.** It emits logs and a run journal; collectors do the rest.
- **Not a bootstrapper.** Quorum services recover themselves from a clean stop. Two
  things electing is how split brain happens.

---

## 2. Where it runs

Two deployments, one binary, differing only in what triggers them.

```
Eaton UPS  (NM2, SNMP, VLAN 10 — every host has a leg on it)
  │
  │  each host: snmp-ups + upsd on localhost
  ▼
  avocado .. eggplant   Proxmox hosts: upsmon secondary + CryoSheep
        │               → gate on UPS, stop guests in order=,
        │                 send the cut, stop self
        │  qm shutdown (ACPI)
        ▼
  k8s VMs, everything else
       systemd + CryoSheep
       → local teardown, stop. Never talks to NUT.
```

### Each host reads the UPS; the last step is the cut

The UPS answers whether the mains are out. Each host answers when it commits and
how it stops itself.

- **Same reading, same verdict.** Every host polls the NM2 with the same
  `override.battery.runtime.low` and the same commit timer, so all five commit
  within a poll of each other without talking.
- **Commit early, stop fast, cut early.** The commit is a fixed time on battery,
  with `runtime.low` as the floor. Charge left when the output goes off stays in
  the battery for the next event and for the restart threshold.
- **The cut is sent last.** Each host reaches the NM2 over its own VLAN 10 leg,
  not through anything the sequence stops, so `shutdown.return <delay>` is the
  step before the halt, at the shared barrier. Nothing is armed while guests can
  still be given back, so a cancel never has to work. Mains back after the cut is
  sent cycles an estate that is already down.
- **No NUT primary.** Every `upsmon` is a secondary: no FSD, no `POWERDOWNFLAG`,
  so NUT's own end-of-shutdown command (`load.off.delay` on this driver: off,
  never back on) is never sent.
- **A host that cannot read the UPS never commits.** The others' early release
  waits on its guests, runs to the ceiling, and their cut catches it. Alert on a
  stale driver before an outage; `wake` reports the cut run after it. The NM2's
  own outage sequence is the backstop for every host dead before the commit.

`nutify` is a read-only client: diagnostics, history, and the operator
notification.

### Reading the estate is not acting on it

A host never stops another host's guests. It may read which guests run anywhere —
from `/etc/pve`, over corosync — to stop waiting early. Stale readings show guests
running after they stopped, so they lengthen a wait and never shorten it.

### Guests do not gate

A guest never decides anything, so it never needs to read the UPS. The host drives
`qm shutdown`; if mains return mid-sequence the **host** abandons and restarts its
guests. This is why guests having no route to `upsd` is not a defect.

The host also never inspects what a guest *is*. It issues `qm shutdown` and honours
the guest's own `down=` budget. Each machine knows how to stop itself.

---

## 3. Triggers

One systemd unit catches every path, because systemd does not care why a unit is
being stopped.

| trigger | verb | halts? |
|---|---|---|
| `upssched`, ONBATT past the linger | `conserve` | no — stops `shed`-tagged guests only |
| `upssched`, ONLINE | `cancel` | no — reverses the event's runs in LIFO order |
| `upssched`, commit timer (90s on battery) | `sleep` | yes, after the UPS cut |
| `upsmon` SHUTDOWNCMD, `runtime.low` floor | `sleep` | yes, after the UPS cut |
| `systemctl poweroff` | `sleep` | systemd halts; CryoSheep must not |
| `systemctl reboot` | `sleep` | **no** — the machine is coming back |
| `qm shutdown` (ACPI from the host) | `sleep` | systemd halts; CryoSheep must not |
| `ansible.builtin.reboot` | `sleep` | **no** |
| boot, however the last stop happened | `wake` | — |

### The halt distinction is load-bearing

`plan.Build` currently appends `host.poweroff` unconditionally. Run as a shutdown
handler during a **reboot**, that powers the machine off instead — a remote reboot
that never returns, and on a guest one that needs a hand on the hypervisor to fix.

CryoSheep must do the teardown and let systemd finish the transition it had already
started. It knows which case it is in from `SERVICE_RESULT`, which systemd passes
only to `ExecStop`. `INVOCATION_ID` does not tell them apart: upsmon's
`SHUTDOWNCMD` inherits it, and must halt.

### Unit shape

```ini
[Unit]
After=network-online.target kubelet.service
Requires=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
ExecStop=/usr/local/bin/cryosheep sleep
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
```

`After=` at startup means **stopped before** at shutdown, so the teardown runs while
the API server is reachable and the mounts still exist.

A second unit runs `cryosheep wake` at boot. It must run however the node went down —
a node cut mid-sequence comes back **still cordoned**, and nothing else will fix that.

`kubectl` is invoked through bare `exec.Command`, so it inherits the ambient
environment. Under systemd there is no `KUBECONFIG` and no `HOME`; the unit must set
it explicitly or cordon and the quorum record silently fail.

---

## 4. Two regimes

The same teardown serves both. Only the evacuation policy differs, and it differs for
a reason.

| | **power event** | **maintenance** |
|---|---|---|
| driver | `upsmon` → CryoSheep | ansible (`maintenance/update-hosts.yml`) |
| scope | the whole estate | one node, `serial: 1` |
| evacuation | **cordon only** | **drain**, PDB-respecting |
| why | nowhere to go — every node is stopping | somewhere to go — the others are up |
| budget | 7m41s of battery | `--grace-period=300 --timeout=600s` |

Draining during a power event would force failovers nobody wanted — moving a database
primary off a node that is stopping, onto a node that is also stopping — and forcing
past a PodDisruptionBudget is how a primary gets killed. Draining during maintenance
is correct for exactly the opposite reason.

**Drain belongs to ansible. Cordon belongs to CryoSheep.** Neither needs the other's
policy.

---

## 5. Ansible integration

**No playbook changes are required.** `ansible.builtin.reboot` stops the machine, the
systemd unit fires, the teardown happens. The integration is that there is no
integration — which is the point.

What CryoSheep contributes to the existing cycle is the part ansible does not do:
releasing CSI mounts and terminating pods in place, so the reboot inside the drain
window does not stall for minutes on mounts whose backing network is already gone.

```
ansible: cordon → drain → os-upgrade → reboot ────┐
                                                   │  systemd stops the unit
                                                   ▼
                          CryoSheep: record → (cordon) → unmount → let systemd reboot
                                                   │
ansible: wait Ready → uncordon ◄───────────────────┘  boot: cryosheep wake
```

### The conflict this creates, and the rule that resolves it

Ansible cordons before CryoSheep ever runs, and uncordons only after
`wait --for=condition=Ready`. If `wake` uncordoned unconditionally at boot, it would
make the node schedulable **before ansible intended**, and would defeat the fail-open
rescue that deliberately leaves a node cordoned when a patch fails.

The rule is the one already stated for the quorum record: **CryoSheep undoes its own
actions and nothing else.**

> If the node was already cordoned when CryoSheep started, CryoSheep did not cordon
> it — so CryoSheep must never uncordon it.

The cordon step records whether it was a no-op, and `wake` and `cancel` skip the
reverse when it was. This resolves the conflict with no coordination between the two
tools, and it generalises: anything CryoSheep finds already in the desired state is
left exactly as it found it.

### Post-maintenance quorum records are empty, and that is correct

On a drained node there are no stateful workloads left to record. An empty record is
an accurate statement about a node that has already been emptied, not a failure.

---

## 6. Observability

CryoSheep logs to stderr. systemd captures it, journald here is persistent, and
collectors read journald. There is no shipping protocol, no spool, and no
`shipped` flag — that was journald reimplemented badly.

The run journal remains, as one thing only: **`calibrate`'s own state**, plus local
evidence readable with everything else down. It is written as it goes and fsynced per
step, so it survives the cut that ends it.

Surfaces, in full, are specified in `Presentation_Layer.md`. In summary: one loud
notification channel carrying three or four messages per event, one muted channel
carrying per-host detail, and a Grafana dashboard that is blank on an ordinary day.

---

## 7. Open decisions

1. **Operator-facing wording** — notification text, `cut`/`graceful` vocabulary,
   channel names. Pending sign-off; `Presentation_Layer.md` is deliberately empty
   until then.
2. **Which guests are `shed`.** The tag exists; no guest carries it yet.
3. **Eaton NM2 polling** — five hosts, nutify and the exporter; confirm the card
   tolerates seven SNMPv3 pollers.
4. **Not yet on hardware:** `shutdown.return` sent by five hosts within seconds,
   the output restoring with utility present, and the core hold releasing from
   the estate reading. Bamboo's full shutdown on 2026-09-28 is the only
   end-to-end run.
