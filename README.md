# CryoSheep

Stops a machine and its guests in order — on power loss, a reboot, or anything
else that asks it to stop.

> **Not GA.** This is being actively experimented with on one estate. Interfaces
> and flags will change without ceremony. The reversible half runs on a real node;
> the irreversible tail — releasing storage mounts, the poweroff itself, and the
> UPS power-cut backstop — has never been executed outside a rehearsal. Read it,
> borrow from it, but do not point it at anything you are not prepared to lose.

<!-- sf:project:start -->
[![GitHub](https://img.shields.io/badge/GitHub-mirror-181717?logo=github)](https://github.com/PrPlanIT/CryoSheep) [![GitLab](https://img.shields.io/badge/GitLab-source-FC6D26?logo=gitlab)](https://gitlab.prplanit.com/PrPlanIT/CryoSheep) [![license](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/license.svg)](https://github.com/PrPlanIT/CryoSheep/blob/main/LICENSE) [![Open Issues](https://img.shields.io/github/issues/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/issues) [![Open PRs](https://img.shields.io/github/issues-pr/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/pulls) [![Contributors](https://img.shields.io/github/contributors/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/graphs/contributors) [![donate](https://img.shields.io/badge/donate-FF5E5B?logo=ko-fi&logoColor=white)](https://ko-fi.com/T6T41IT163) [![sponsor](https://img.shields.io/badge/sponsor-EA4AAA?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/PrPlanIT)
<!-- sf:project:end -->
<!-- sf:badges:start -->
[![release](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/release.svg)](https://github.com/PrPlanIT/CryoSheep/releases) [![build](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/build.svg)](https://gitlab.prplanit.com/PrPlanIT/CryoSheep/-/pipelines) [![Go Version](https://img.shields.io/github/go-mod/go-version/PrPlanIT/CryoSheep?logo=go)](https://github.com/PrPlanIT/CryoSheep) [![Go Reference](https://pkg.go.dev/badge/github.com/PrPlanIT/CryoSheep.svg)](https://pkg.go.dev/github.com/PrPlanIT/CryoSheep) [![Last Commit](https://img.shields.io/github/last-commit/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/commits) [![StageFreight](https://img.shields.io/badge/StageFreight-0.12.0--dev+e2c558a-310937?logo=readthedocs&logoColor=white)](https://stagefreight.prplanit.com)
<!-- sf:badges:end -->

A UPS gives you minutes, and a stock `poweroff` spends them badly. A hypervisor
halted without asking yanks its guests rather than stopping them, so nothing
inside ever receives `SIGTERM`. A Kubernetes node stalls for minutes on CSI mounts
whose backing network is already gone, then gets reset. Both produce the unclean
stop that makes recovery unsafe.

CryoSheep answers exactly one question — **how does this machine stop cleanly** —
and leaves the other two to whoever already owns them:

| question | owner |
|---|---|
| are the mains out? | the UPS, via NUT |
| should this machine stop now? | `upsmon`, systemd, or an operator |
| **how does it stop cleanly?** | **CryoSheep** |

It is invoked, never resident, and strictly local: it never reaches across a host
boundary. A sequencer that shuts down its own host cannot finish a loop over
others, and a tool that can stop machines it does not live on can stop the wrong
one.

## What it does

Records which workloads held authority while the cluster is still whole. Cordons
the node — **never drains it**, because in a full shutdown there is nowhere for a
database primary to go except another machine that is also stopping. Releases CSI
mounts before the network disappears, which is what a stock shutdown stalls on.
Stops guests over ACPI (`qm shutdown`, not `qm stop`) in **reverse Proxmox
`startup: order=`**, honouring each guest's own `down=` budget. Arms the UPS's own
power-off first, as a backstop for a sequence that cannot finish.

And it undoes all of it if mains return before the point of no return.

## Install

```sh
curl -fsSL -o /tmp/cryosheep.tar.gz \
  https://gitlab.prplanit.com/PrPlanIT/CryoSheep/-/releases/latest-dev/downloads/CryoSheep-latest-dev-linux-amd64.tar.gz
tar xzf /tmp/cryosheep.tar.gz -C /usr/local/bin cryosheep
rm /tmp/cryosheep.tar.gz
cryosheep version
```

Replace `latest-dev` with a release tag for a pinned build.

The binary alone does nothing until something invokes it. See [`deploy/`](deploy/)
for the two systemd units and the `upsmon` wiring.

## Start here

`plan` reads the host and prints what would happen. It changes nothing, needs no
configuration, and is safe on a production machine:

```sh
cryosheep plan
```

It reports the roles it detected, every guest with its startup order, the ordered
sequence, and where the point of no return falls.

To rehearse without acting — including the shutdown-handler path, which is the one
that runs most often and that nobody can watch while it happens:

```sh
INVOCATION_ID=rehearsal cryosheep sleep --dry-run
```

## Commands

| | |
|---|---|
| `plan` | show what would happen; changes nothing |
| `conserve` | shed load and hold, reversibly — `upsmon` `NOTIFYCMD`, `ONBATT` |
| `cancel` | abandon a sleep and undo it — `NOTIFYCMD`, `ONLINE` |
| `sleep` | the whole sequence — `SHUTDOWNCMD`, or a unit's `ExecStop` |
| `wake` | revive after stasis — on boot |
| `calibrate` | recommend a deadline from what past runs actually cost |

Three flags, all optional:

| | |
|---|---|
| `--dry-run` | walk and record the decisions without performing them |
| `--ups-deadline` | when the UPS cuts power regardless — `CRYOSHEEP_UPS_DEADLINE` |
| `--kubeconfig` | identity for `kubectl` — `CRYOSHEEP_KUBECONFIG` |

Everything else is either a constant or something another system already owns: the
UPS connection is read from `/etc/nut/upsmon.conf`, the per-guest budget from
Proxmox `startup: down=`, and why the machine is stopping from the environment of
whatever invoked it. A flag that can contradict the truth is worse than no flag.

## What triggers it

Installed as a systemd unit, the teardown runs however the machine is stopped:

| | |
|---|---|
| `systemctl poweroff` | teardown, then systemd halts |
| `systemctl reboot` | teardown, then systemd reboots — **no poweroff** |
| `qm shutdown` from the hypervisor | teardown, then systemd halts |
| `ansible.builtin.reboot` | teardown, then reboot |
| `upsmon` `SHUTDOWNCMD` | full sequence, including poweroff |
| `systemctl stop cryosheep-stasis` | **nothing** |

That last row is deliberate. `ExecStop` fires whenever the unit stops, so stopping
it by hand would otherwise cordon a healthy node and release its mounts. CryoSheep
asks systemd whether a shutdown is actually in progress and declines when it is not.

## How it decides

Four rules, each because the alternative fails badly during a power cut.

**The sequence always reaches the end.** A step that errors is recorded and the
walk continues — a host left running until the battery is flat is worse than any
single step failing.

**Only a positive reading aborts.** Mains observed back before the point of no
return abandons the sequence and reverses it. Being *unable to read* the UPS
changes nothing in either direction: blindness must never alter a decision.

**Gates stop once committed.** They run before every reversible step and before
the committing one, then cease. Re-checking after that could only add a way to hang.

**It undoes its own actions and nothing else.** A `noout` or a cordon an operator
set by hand is not CryoSheep's to clear — which is also what keeps it out of the
way of a maintenance playbook that cordons first.

## What it records

Structured lines on stderr. systemd captures them, and every line is forced to
disk with an explicit journal sync before the next step begins — journald's own
sync interval is five minutes, and a power cut inside that window loses exactly
the tail worth having.

There is no second copy and no file of its own. A parallel record with its own
rotation is one that nothing ships and nobody reads.

```sh
journalctl -t cryosheep -b -1     # the previous boot, on the machine itself
```

A run that was cut leaves a `step.start` with no `step.end`. That absence is the
evidence, and `wake` reports it on the way back up.

At a terminal the same run reads as prose rather than JSON:

```
09:24:42  quorum.record      dungeon-chest-004
09:24:42                     recorded 33 workloads
09:24:42                     done        382ms
09:24:42  cordon             dungeon-chest-004
09:24:42                     skipped       4ms
09:24:42  run                complete in 434ms
```

## Calibration

The UPS deadline should be measured, not chosen. It sits between two failure
modes: too short cuts power mid-flush and produces the unclean stop everything
else exists to prevent; too long and the battery is flat before it fires.

```sh
cryosheep calibrate
```

It reads past runs back out of the journal, recommends from the worst observed run
plus a margin, excludes runs that were cut short, and refuses a recommendation
that does not fit the battery. Runtime rather than charge percent, because runtime
already folds load in.

## Uninstall

```sh
systemctl disable --now cryosheep-stasis.service cryosheep-wake.service
rm -f /etc/systemd/system/cryosheep-stasis.service /etc/systemd/system/cryosheep-wake.service
rm -f /usr/local/bin/cryosheep
systemctl daemon-reload
```

It keeps no state of its own — the record lives in journald under journald's own
retention. If you wired it into `upsmon`, remove that too; CryoSheep does not
install it and will not remove it:

```sh
grep -rl cryosheep /etc/nut 2>/dev/null
```

## License

AGPL-3.0-only.
