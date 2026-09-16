

1. We need to ensure we can restore any actions beneath that are taken during a power outage that does not last long enough that we are able to reverse cancel the shutdown actions/plans.

┌───────────────────────┬──────────────────────────────────┐
│        action         │            reversed?             │
├───────────────────────┼──────────────────────────────────┤
│ UPS deadline armed    │ yes — cancelled first            │
├───────────────────────┼──────────────────────────────────┤
│ guests stopped        │ yes — restarted from the journal │
├───────────────────────┼──────────────────────────────────┤
│ ceph osd set noout    │ yes — unset                      │
├───────────────────────┼──────────────────────────────────┤
│ nodes cordoned        │ no — not built                   │
├───────────────────────┼──────────────────────────────────┤
│ workloads scaled down │ no — not built                   │
> Scenario just for reminder:  Our case is we dont want partial shutdown case where the shutdown starts after 2-3 minutes, The power comes on at 4 minutes etc, Operator goes phew, nothing actually went out thats convenient, no downtime. Continues working on stuff. Hears a click, the Node actually just reset... Everything else did too. Awww shit... DOWNTIME (Awful scenario, that has happened many times at random!)

2. Normal shutdowns - k8s shutdowns with stock kubeadm + Ubuntu/Debian it tends to stall during poweroff and take much longer than expected to clear... There is attempts at resolving this in Dungeon repository within the ansible playbooks I believe as well as we did fine with the ansible kubelet update script in handling some of the shared problem space perhaps.
Hitting power off in proxmox dashboard fpr the k8s nodes pending this work we hope should do the right thing with our kubeadm Ubuntu/Debian cluster and not stall for many many minutes and eventually failover to HARD RESET, should be graceful...

- Theme: CryoSheep is a play on the idea of CrioSleep. As if we are counting sheep as the power goes out to hibernate them in cryogenic sleep chambers.
We run production workloads, stopped is never the desired state, here we do not think in shutdown, we put things in stasis, or conserve energy so they are safe during the event and then wake them up after its resoved.
As such for shutdown we simply have `cryosheep sleep`, and to cancel a shutdown if we scheduled or want to abort etc `cryosheep cancel`. This philosophy is also why it is important we orchestrate revival when sleep is no longer needed.
