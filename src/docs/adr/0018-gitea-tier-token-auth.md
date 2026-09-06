# ADR-0018: Gitea auth & egress — hive-held tier tokens behind an enforcing proxy host

Status: Proposed

## Context

Gitea support (#6167) needs a decided answer to "what is the credential, who
holds it, and how does it reach a request" before any Gitea write path can be
built (#6169). GitHub answers this with machinery Gitea does not have: a GitHub
App whose per-agent installation tokens are minted fresh at a tier chosen by the
agent's ACMM mode (`AgentMode.TokenTier()`, `pkg/agent/mode.go:64-77`), expire
in an hour, and are delivered as per-agent `0640` cache files whose *path* — not
value — rides the agent environment (`HIVE_AGENT_TOKEN_CACHE`,
`pkg/agent/manager.go:9355-9357`; `pkg/github/app.go:440-512`). Gitea has no App
or installation-token concept; what it has, since 1.20.0 (go-gitea/gitea#24767),
is user-owned access tokens carrying categorized scopes such as
`read:repository` and `write:issue`.

The tracker's sketch was: one bot account plus operator-created tier-scoped
tokens, "injected via the existing MITM proxy (`RegisterGitHubHost`-style
registration)" so agents never hold a credential, with push served by the
credential helper. Reading the code before deciding turned up four facts that
reshape that sketch, and this record is written against them:

1. **The proxy injects nothing today.** The MITM relay forwards requests
   verbatim (`req.Write(upstream)`, `pkg/proxy/github_proxy.go:1036`); GitHub
   auth is attached by the `gh` wrapper (`bin/gh-wrapper.sh:96-114`) and the git
   credential helper (`bin/git-credential-hive.sh:135-171`) from the per-agent
   cache file. Proxy-side injection is deferred work (#1861,
   `github_proxy.go:731-735`). So on GitHub, agents *do* briefly hold a
   credential — a short-lived, tier-scoped, per-agent one. Gitea's tokens are
   long-lived, which changes what "briefly hold" costs.
2. **`RegisterGitHubHost` does not enforce.** Registration only extends
   `IsGitHubHost`; `NeedsMITM` is a literal check on `api.github.com` plus the
   Linear host (`pkg/proxy/rules.go:78-80`), so a registered GHE host is
   tunneled opaquely, never inspected (`rules_test.go:199-214`). The function's
   own doc comment overstates this. The worked precedent for actually enforcing
   a non-GitHub host is Linear (`pkg/proxy/linear_rules.go`), and
   `NeedsInspection`'s comment documents the seam: a host must be added to the
   MITM set to be inspected (`rules.go:83-95`).
3. **Traffic to any uninspected host is an opaque tunnel** — CONNECT
   passthrough to any host and port (`github_proxy.go:812-819`, `:1237-1252`),
   with the iptables backstop redirecting only `:443` for non-root, non-proxy
   UIDs (`src/deploy/entrypoint.sh:1582-1587`) and rejecting IPv6 `:443`
   outright (`:1744-1745`). A Gitea host is unenforced by default —
   `forge-app-setup.md` already flags this as a reason the adapter path is not
   deployment-ready.
4. **A `GITEA_TOKEN` environment variable would leak into every agent
   session.** Agents are tmux panes inside the hive container, and the tmux
   server inherits the hive process environment; secrets are kept out by an
   explicit *denylist* (`pkg/agent/manager.go:2246-2259`, `filteredEnv`
   `:9082-9095`, `bin/agent-env-scrub.sh:42-48`) that names only GitHub and
   Linear variables. The env var the current `gitea.token_env` config points at
   is not on any strip list.

**Scope.** This record decides the credential and egress model. It changes no
behavior: no config key is added, no proxy rule ships, no watcher grows a Gitea
backend. Those are Wave 1+ slices under #6167 and are unblocked by this.
Enumeration neutralization is a separate record (#6170).

## Decision

**One dedicated bot account per Gitea instance. Three operator-created scoped
tokens on that account — one per existing token tier. The hive process is the
only holder of those tokens: they are consumed server-side by the request-file
relays, and reach agent-initiated traffic only by injection inside the MITM
proxy, which gains the Gitea host as its third enforced host. No Gitea
credential is ever written into an agent session, an agent-readable file, or
the credential helper's answer set.**

### The tier table

The tier vocabulary is the one the GitHub App path already uses
(`TokenTier()`, `pkg/agent/mode.go:64-77`), so one mode value keeps driving
every layer:

| ACMM mode | Tier | Gitea token scopes | Operations it covers |
| --- | --- | --- | --- |
| `ADVISORY` (and `NO_GITHUB`) | `advisor` | `read:repository`, `read:issue`, `read:user` | API reads, `git clone`/fetch |
| `ISSUES_ONLY` | `newcomer` | advisor + `write:issue` | issues, comments, labels, hold |
| `ISSUES_AND_PRS` | `contributor` | newcomer + `write:repository` | `git push`, PR creation |
| `ISSUES_PRS_MERGE` | `trusted` | same token as `contributor` | merge |

Three tokens, four modes, deliberately. Gitea routes pull-request creation *and*
merge through the `repository` scope category (Gitea docs; confirmed for git
smart HTTP in `services/context/permission.go:62-82` +
`routers/web/repo/githttp.go:153`, Gitea v1.24 source), so no token can grant
"open PRs but not merge". The holdgated/full distinction therefore stays where
it already lives on GitHub: the relays (`AuthorizeMerge` → `CanMerge()`,
`pkg/agent/manager.go:8974-9001`; SHA-pinned merge-eligibility,
`cmd/hive/main.go:8318-8343`) and the proxy's every-mode hard-deny of direct
PR-create and merge routes (`pkg/proxy/rules.go:211-223`). This is not a
regression from GitHub: the GitHub `contributor` App token already carries the
permissions a merge call needs, and the same relays and denies are what stand
in the way there too.

Advisory agents get real read credentials, as they already do on GitHub (the
`advisor` App tier grants `contents:read`/`metadata:read`/`pulls:read`,
`pkg/github/app.go:324-335`). On a private Gitea instance — the motivating
deployment — unauthenticated reads fail, so an advisory tier without a read
token would not be "safe", it would be blind.

### Where the tokens live

- Named by per-tier environment-variable names in the `gitea:` config block
  (exact key naming is Wave 1 config design; the existing single `token_env`
  remains valid as the write-tier name for the adapter's current caller). The
  values live in the deployment's environment or secret files, never in
  `hive.yaml` — the existing no-secrets-in-config rule.
- **Wave 1 must add every Gitea token variable to all three strip layers**
  (`agent-env-scrub.sh`, the `set-environment -u` block at
  `manager.go:2246-2259`, and `filteredEnv` at `:9082-9095`) before any other
  Gitea slice lands, because finding 4 above means the leak is the default.
- The per-agent cache-file delivery pattern is deliberately **not** reused for
  Gitea. It is the right shape for credentials that expire in an hour; serving
  a non-expiring PAT through it would hand every write-capable agent a durable
  credential whose only revocation is operator rotation.

### The egress side: the Gitea host becomes the third enforced host

Wave 1 registers the host from `gitea.gitea_url` the way GHE hosts are derived
from `github.api_url` (`cmd/hive/main.go:3476-3482`) — but registration alone
tunnels (finding 2), so the host is also added to the MITM set via the
`NeedsInspection` seam, with a Gitea rule table shaped like Linear's
(`linear_rules.go`): first-match-wins `(method, path) → MinMode` over
`/api/v1`, deny-unknown-writes by default, the existing repo allowlist applied,
and every-mode hard-denies for `POST /api/v1/repos/*/*/pulls` and
`POST /api/v1/repos/*/*/pulls/*/merge` with directives pointing at
`hive-open-pr` / `hive-merge`, mirroring `rules.go:211-223`. Git smart HTTP on
the same host is classified too: `git-upload-pack` at `ADVISORY`,
`git-receive-pack` at `ISSUES_AND_PRS`.

Inside that MITM, the proxy attaches the credential: it classifies the request,
selects the *lowest* tier token that satisfies the classification (reads →
advisor, issue writes → newcomer, repo writes and receive-pack → contributor),
verifies the agent's mode meets the rule's `MinMode`, strips any agent-supplied
`Authorization` header toward the Gitea host, and injects
`Authorization: token <tier PAT>`. Agent identity keys off the connection's
owning UID as it does today (`github_proxy.go:762-783`); the self-asserted
fallback stays disabled by default (`:753-758`).

Why injection is decided here when #1861 defers it for GitHub: the deferral is
about turning a *spoofable* identity into a freshly *minted* token. Here there
is no mint — the worst a confused injection can do is attach a static tier
token the agent's own mode already entitles it to, the mode itself is read from
root-owned state, and least-privilege-per-request means a read is never carried
by a write token regardless of who asks.

Operator-facing consequences of riding the existing gate: the instance must be
reachable over HTTPS on port 443 via IPv4 (the iptables backstop redirects only
`:443` and IPv6 `:443` is closed — finding 3; a plain-HTTP or odd-port
`gitea_url` should fail config validation in Wave 1 rather than silently
bypassing enforcement), and the hive MITM CA must verify for the Gitea host in
every agent CLI. The CA mechanics do not change — `forgeCert` signs leaves for
any host under the one installed CA (`github_proxy.go:1445-1508`) — but the
Copilot pinning history (`src/Dockerfile:204-217`) says per-CLI verification is
empirical, not assumed.

### Push and reads follow from the same move

- **Push**: agents run `git push` with no credential; the request transits the
  MITM'd host, is gated at `ISSUES_AND_PRS`, and gets the contributor token
  injected. The credential helper is **not** registered for the Gitea host.
  The evidence question in #6169 — could `git-credential-hive.sh` serve a
  second host without disturbing GitHub — is answered yes (the helper echoes
  whatever host it is registered for, pinned by
  `bin/test_git_credential_hive.sh:134-136`; only registration in
  `entrypoint.sh:343-378` and the `x-access-token` username convention are
  GitHub-specific), and then deliberately not used, because everything the
  helper returns is readable by the agent that invoked it.
- **Agent reads**: plain HTTPS to `/api/v1` through the proxy, authenticated by
  injection. No `tea`, and no bespoke read CLI in this record. `tea` wants its
  own on-disk login (a token in agent-readable space — the thing this design
  exists to avoid) and brings a second command surface needing its own
  wrapper-style deny model. A convenience read CLI can be added later without
  touching this model, since it would ride the same proxy; rejecting it here
  costs nothing.

### Operator expectations: creation, storage, rotation

- Create the bot account per instance; make it a collaborator with write
  permission only on the repos the hive manages (`project.repos` remains the
  hive-side allowlist, `rules.go:300-312`). No site-admin. Enable 2FA on the
  account — Gitea exempts API tokens and git-over-HTTP token auth from the 2FA
  basic-auth block, so this costs the hive nothing and closes password/UI abuse.
- Create the three tokens with exactly the scopes in the table. Do not use the
  public-only modifier (hive repos may be private). On Forgejo, additionally
  restrict each token to the specific repositories the hive manages (a Forgejo
  capability Gitea lacks); and never substitute an OAuth2-application token —
  Forgejo documents those as unscoped.
- Rotation is regenerate-in-UI, update the deployment environment, restart the
  hive. These tokens do not expire, so rotation is calendar- and
  incident-driven: rotate on a schedule the operator sets (quarterly is a
  reasonable default), and immediately on any agent-compromise signal or
  departure of anyone who could read the deployment environment. Gitea's
  token last-used timestamps are the audit trail for a token that should have
  been rotated.
- All forge actions appear as the one bot identity. That matches GitHub's
  App-authored behavior; per-agent attribution stays in the relay-stamped
  request metadata and hive's own audit logs, not in forge identities.

### Rejected: register-and-tunnel with wrapper/helper-served tier tokens

The closest translation of today's GitHub mechanics — hand each agent its tier
PAT via the existing per-agent file, let a wrapper and the helper attach it,
tunnel the Gitea host — fails on the interaction of two facts: the tokens are
durable, and the tunnel is blind. A holdgated agent holding the contributor PAT
could call the merge API directly, and nothing on the wire would stop it; on
GitHub that same gap is closed by the proxy's every-mode merge deny on
`api.github.com`. Egress enforcement for the Gitea host is therefore not
optional the moment any write-capable credential is agent-reachable — and once
the host is MITM'd for enforcement, injection costs little and removes the
agent-held credential entirely. Choosing enforcement-without-injection would
keep the wrapper/helper plumbing and the leak surface while already paying the
MITM's costs; choosing both halves is the smaller total system.

### Rejected: minted short-lived Gitea tokens

Gitea can create tokens over its API, but only under basic auth with the
account password, or wielding an `admin`-scoped credential with sudo. Either
parks a strictly more dangerous secret in the hive to save a less dangerous
one. The ADR-0007 mint is unaffected and orthogonal — its JWTs already use the
same tier vocabulary (`pkg/mint/agent.go:43-48`) and it never touches forge
credentials. Reopen this if Gitea grows an installation-token analogue.

### Rejected: per-agent bot accounts

N accounts multiply rotation, membership, and collaborator management per
agent roster change, for an attribution benefit hive already provides in its
own audit trail. Nothing in the gathered evidence forces them. Reopen if
forge-side per-agent identity becomes a hard requirement (e.g. branch
protections keyed to distinct authors).

## Evidence grading

In the ADR-0017 manner: what is measured, what is derived, what is not yet run.

| Claim | Grade |
| --- | --- |
| Unregistered-host traffic is tunneled untouched; only `api.github.com` and the Linear host are inspected | **Read from source** (`rules.go:78-95`, `github_proxy.go:812-819`) and pinned by tests (`rules_test.go:199-214`); **not yet reproduced from an agent UID in a live container** — no live deployment was reachable from this working environment |
| iptables backstop is 443-only, IPv6 443 closed, exemptions root/proxy-UID/mark | Read from source (`entrypoint.sh:1562-1610`, `:1723-1764`); same live caveat |
| Proxy performs no auth injection on any host today | Read from source (grep over `pkg/proxy`; `github_proxy.go:1036`) |
| A `GITEA_TOKEN` env var reaches agent panes absent strip-list changes | Derived from the denylist design (`manager.go:2246-2259`, `agent-env-scrub.sh:42-48`); not demonstrated live |
| Helper serves arbitrary registered hosts unchanged | Pinned by an executing test (`test_git_credential_hive.sh:134-136`) |
| Gitea scope model: categorized scopes since 1.20.0; PR create and merge under `write:repository`; git HTTP requires repository-category scope at the operation's level | Primary sources: go-gitea/gitea#24767 (milestone 1.20.0); Gitea docs; Gitea v1.24 source (`services/context/permission.go:62-82`, `routers/web/repo/githttp.go:149-156`) |
| Forgejo: same PAT scope model, plus per-repository token restriction; OAuth2 tokens unscoped | Forgejo documentation only; validate against Codeberg/Forgejo in Wave 3 |
| MITM CA verifies for a second host in every agent CLI in use | **Not verified**; mechanism read from source (`forgeCert`), per-CLI behavior is a Wave 1 validation item (the Copilot pinning history is the cautionary precedent) |
| Git packfile traffic through the MITM relay is operationally sound | **Not verified**; Wave 1 validation item |

The two "empirical, from an agent UID inside a live container" probes #6169
asked for remain open as Wave 1 entry criteria; this record states what the
code says will happen and must be re-checked against a live hive before the
proxy slice merges.

## What would reopen this decision

1. **A live probe contradicting the code-derived egress behavior** — e.g. an
   agent-UID connection to an unregistered host being blocked rather than
   tunneled, or the mark/UID exemptions not behaving as read.
2. **Packfile traffic through the MITM relay proving unworkable** (memory,
   latency, or protocol breakage on large pushes/clones). The recorded
   fallback is helper-served write-tier tokens for git only — accepting an
   agent-held durable credential for push, with the API side still
   proxy-injected — and that trade must come back through an ADR, not slip in
   as a hotfix.
3. **A CLI in the agent roster that cannot trust the hive CA for the Gitea
   host** (the Copilot scenario recurring), forcing a per-CLI exception that
   weakens the single-enforcement-point property.
4. **Gitea growing app-style installation tokens**, which would let the mint
   pattern replace static PATs.
5. **A hard requirement for forge-side per-agent identity**, reopening the
   per-agent-bot-accounts rejection.

## Consequences

- Agent sessions on a Gitea hive hold no forge credential at all — a stronger
  property than the GitHub path has today, chosen deliberately because Gitea's
  tokens are durable where GitHub's are hourly.
- The proxy becomes load-bearing for Gitea in a way it is not for GitHub git
  traffic (which tunnels to `github.com`): API *and* git for the Gitea host
  transit the MITM. Enforcement and availability now share a component; the
  proxy being down means a Gitea hive's agents cannot read the forge.
- Wave 1 inherits a concrete work list from this record: strip-list additions
  first; host registration that actually feeds the MITM set (and fixing
  `RegisterGitHubHost`'s doc comment while there); a `gitea_rules.go` in the
  Linear shape; injection with Authorization-stripping; config validation
  rejecting non-HTTPS/non-443 `gitea_url`; per-tier token env names; the two
  live probes and the packfile/CA validation items above.
- The relays stay the only write path on every forge, so the watcher slices
  (#6167 Waves 1–3) target the forge seam without revisiting auth.
- Operators get a three-token, one-bot setup with UI-driven rotation — more
  manual than the GitHub App, and honest about it: the rotation section above
  is the cost of having no installation-token analogue.
- `NO_GITHUB` and `ADVISORY` intentionally share the advisor tier; the shell
  wrapper's stricter `NO_GITHUB` arm (`gh-wrapper.sh:577-582`) is a
  GitHub-CLI concern and does not gain a Gitea analogue in this record.

## References

- [#6169](https://github.com/hivecommons/hive/issues/6169) — this task;
  [#6167](https://github.com/hivecommons/hive/issues/6167) — the Gitea series
  and wave plan; [#6170](https://github.com/hivecommons/hive/issues/6170) —
  enumeration neutralization (separate record).
- [ADR-0002](0002-mitm-proxy-network-enforcement.md) — the proxy this record
  extends; [ADR-0003](0003-acmm-autonomy-levels.md) — the mode dial;
  [ADR-0005](0005-forge-abstraction.md) — the adapter seam;
  [ADR-0007](0007-token-mint.md) — the mint, unaffected here.
- [Forge setup: GitLab, Gitea, and Forgejo](../forge-app-setup.md) — the
  candid support matrix this record starts from, including gap #4 (credential
  plumbing) and the tunneled-host caveat.
- go-gitea/gitea [#24767](https://github.com/go-gitea/gitea/pull/24767) —
  scoped-token redesign, milestone 1.20.0; Gitea v1.24 source for the git-HTTP
  scope check; [Forgejo token-scope documentation](https://forgejo.org/docs/latest/user/token-scope/).
