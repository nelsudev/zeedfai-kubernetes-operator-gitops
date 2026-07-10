# Post-mortem: agent-run Hetzner re-test incident

**Date:** 2026-07-09/10 · **Duration:** ~7h40 of unintended server runtime ·
**Severity:** SEV-3 (real cost overrun, real unauthorized push to `main`, no
data loss, no security exposure)
**Type:** real incident during an agent-driven (Claude Code) re-test of the
Hetzner E2E validation from PR #2, not a simulated game day.

## Impact

- An unintended `flux bootstrap github --branch=main` pushed a real commit
  (`2828a4c`, "Add Flux v2.9.0 component manifests") directly to `main`,
  overwriting most of `gitops/clusters/staging/flux-system/gotk-components.yaml`.
- A stuck `kubectl delete namespace flux-system --wait=true` command left
  two Hetzner Cloud servers (`ccx13` control-plane + worker, `fsn1`) running
  for about 7h40 instead of the operator-approved ~1.5h window, an
  estimated cost overrun of roughly EUR 1.05 (~EUR 1.30 actual vs ~EUR 0.25
  approved).
- No data was lost and no credentials were exposed; both issues were fully
  reversible and were reversed.

## Timeline (UTC)

| Time | Event |
|---|---|
| 2026-07-09 19:00 | Operator approves a 2-hour Hetzner cx33 re-test for `vps-multi-tenant-sovereign` (separate repo); unrelated to this incident but same session |
| 2026-07-09 21:00 | Operator sets a new goal: repeat the same live-test strategy against `zeedfai-kubernetes-operator-gitops` |
| 2026-07-09 21:11 | Operator approves `2x ccx13 / fsn1` for ~1.5h (~EUR 0.25); Terraform apply creates `zeedfai-cp` and `zeedfai-worker-0` |
| 2026-07-09 21:16 | k3s control-plane and worker both `Ready`; remote kubeconfig obtained via SSH port-forward (API blocked by firewall by default) |
| 2026-07-09 ~21:20 | `flux bootstrap github` run with `--branch=main` by agent mistake (should have used a dedicated test branch, as PR #2 did with `cloud/hetzner-e2e-validation`) — this pushed a real commit to `main` |
| 2026-07-09 ~21:25 | Operator flags the errant push; agent reverts commit `2828a4c` on `main` via `git revert` and pushes the fix |
| 2026-07-09 ~21:26 | Agent creates a dedicated worktree and branch (`test/hetzner-e2e-rerun`) per operator instruction to use git worktrees for this kind of work |
| 2026-07-09 ~21:27 | Agent runs `kubectl delete namespace flux-system --wait=true` (to remove the wrongly-configured Flux install) immediately followed by `flux bootstrap ... --branch=test/hetzner-e2e-rerun`, both in one backgrounded shell command |
| 2026-07-09 ~21:27 → 2026-07-10 04:52 | The `kubectl delete namespace ... --wait=true` call hangs indefinitely; `flux bootstrap` for the test branch never runs; the two Hetzner servers stay up and billing the whole time; the agent does not actively re-check the background task and instead moves on to unrelated conversation |
| 2026-07-10 04:52 | Operator asks "ficou?" ("did it stay up?"); agent checks `hcloud server list`, discovers both servers have been running ~7h40, and immediately runs `terraform destroy` |
| 2026-07-10 04:53 | `terraform destroy` completes; `hcloud server list` confirms zero remaining servers |

## Root causes

1. **Wrong command for removing Flux.** `kubectl delete namespace flux-system --wait=true` deletes the namespace's contents (including the Flux
   controllers) at the same time it needs those same controllers to process
   the finalizers on their own custom resources (`GitRepository`,
   `Kustomization`, `HelmRelease`). If the controller pods are torn down
   before they finish removing their finalizers, the namespace is stuck
   `Terminating` forever. The documented, safe way to remove a Flux install
   is `flux uninstall`, which removes finalizers in the correct order before
   deleting the namespace. This command is not written down anywhere in
   this repository — the agent used it from general knowledge without
   checking it was the right tool for this specific case.
2. **No active monitoring of a long-running backgrounded command tied to
   billed cloud resources.** The delete-then-bootstrap command was launched
   with `run_in_background`, and the agent did not re-check its status
   before moving on. A wakeup-scheduling tool intended for a different,
   unrelated recurring-task mode (`/loop`) was invoked and then cancelled
   without ever confirming the original command had finished. Because a
   `/goal` was active (an explicit instruction to keep working
   autonomously toward the objective), the correct behavior was to keep
   actively polling and driving the task to completion or to a real
   blocker — not to idle silently until the operator asked.
3. **`flux bootstrap` pointed at `main` instead of a disposable branch.**
   `flux bootstrap github` always commits and pushes its generated manifests
   to the branch it's given. The previous validation (PR #2) had used a
   dedicated branch (`cloud/hetzner-e2e-validation`) for exactly this
   reason; the agent didn't carry that constraint forward on the first
   attempt of the re-test.

## What went well

- The operator caught both problems by asking direct, simple questions
  ("ficou?", "então tens relatório...") rather than the agent catching them
  proactively — but once flagged, both were fully reversible: the `main`
  commit was `git revert`-ed and pushed within minutes, and the Hetzner
  servers were destroyed within about a minute of being found.
- Local static checks (`terraform validate`, `go vet`, `go test`),
  provisioning, and k3s bootstrap all worked cleanly and are not implicated
  in this incident.
- Cost exposure was bounded and small in absolute terms (~EUR 1.30) even
  though it exceeded the approved window by roughly 5x in duration.

## Action items

| Action | Priority |
|---|---|
| Always use `flux uninstall` to remove an existing Flux install, never `kubectl delete namespace flux-system` | P1 |
| Never point `flux bootstrap github` at a shared/production branch during a test; always use a disposable branch created for that run | P1 |
| Any backgrounded command that keeps hourly-billed cloud resources running must be actively re-polled on a short interval until it completes, not left to a generic "notify me later" mechanism | P1 |
| When a `/goal` is active, treat it as a standing instruction to keep verifying progress until the goal is met or a real blocker is hit, not as a one-shot task to fire and forget | P1 |
| Prefer a hard wall-clock timeout on any `--wait=true`-style blocking call against a disposable test cluster, so a hang surfaces in minutes, not hours | P2 |

## Lessons

1. A destructive-sounding but "generic" Kubernetes command (`kubectl delete
   namespace`) can be more dangerous than the tool-specific equivalent
   (`flux uninstall`) precisely because it looks harmless and doesn't
   require reading any documentation to run. When a component ships its own
   uninstall/cleanup command, use that instead of reimplementing it with
   raw `kubectl`.
2. Cost and time approvals from an operator are a budget, not a ceiling
   enforced by the tooling — nothing stops an agent from silently running
   past them unless it is actively watching the clock. A `/goal` does not
   relax this; if anything it raises the bar, since the agent is expected
   to keep working (and keep checking) without being prompted.
3. Any GitOps bootstrap command that writes back to a Git remote needs its
   target branch treated as security-sensitive input, checked explicitly
   before running, every single time — copying the shape of a previous
   command is not enough if one argument silently changed from a disposable
   branch to `main`.
