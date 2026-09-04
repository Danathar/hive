# dibs.hivecommons.dev domain cutover runbook

Issue [#5925](https://github.com/hivecommons/hive/issues/5925) moves `dibs` to
`https://dibs.hivecommons.dev` and demotes `https://dibs.kubestellar.io` to a
redirect. This is repo-side preparation only: do not touch live DNS, Kubernetes,
cert-manager, or releases until an operator intentionally executes the sequence.

## Current repo inventory

- `dibs.kubestellar.io` appears only in hub SSO comments/tests under `src/pkg/hub`;
  no canonical docs link was found in `README.md`, `docs/`, `src/docs/`,
  `dashboard/`, `examples/`, `discord/`, `config/`, or `.github/`.
- Searching `src/` for `dibs` found the hub-side `/api/saas/dibs/repos` feed and
  SSO bridge only. No `dibs` URL environment default or constant is configured in
  this repository.
- `hivecommons/infra` contains shared CI/Prow files only, and
  `hivecommons/hive-redirect` is a GitHub Pages redirect for the hub entry point.
  The live `dibs/dibs` Ingress and `hive-hub/hive-wildcard-tls` Certificate were
  not found in git, so staged Kubernetes manifests live under
  `src/deploy/dibs-domain-cutover/`.

## Ordered operator sequence

0. **Preflight — do this now, not on the day.** Every assumption the staged
   manifests rest on is checkable read-only, at zero certificate cost:

   ```sh
   bash src/deploy/dibs-domain-cutover/preflight.sh
   ```

   Run it as soon as you have cluster access, well before the hold expires. The
   expensive failure in this sequence is not "a step fails" — it is "a step
   fails *after* the certificate has been re-issued", because that spends the
   window the whole plan is waiting for and the next attempt is a week out. The
   preflight exits `78` when something would block, and it treats a check it
   could not run as a warning, never as a pass.

   It verifies, in particular, the assumption step 4 depends on and that nothing
   else in this repo does: that the ingress-nginx controller's
   `--default-ssl-certificate` really is `hive-hub/hive-wildcard-tls`. If it is
   not, `dibs.hivecommons.dev` is served ingress-nginx's **self-signed** default
   certificate — a host that answers `200` while failing certificate validation,
   discovered only at step 5, with the issuance already gone. Every other
   Ingress in this repo names a `secretName` explicitly
   (`src/pkg/hub/saas_provision.go`); this one cannot, because an Ingress may
   only reference a Secret in its **own** namespace and the wildcard lives in
   `hive-hub`. That is why the default-certificate path is load-bearing here and
   worth confirming in advance.

1. **DNS:** In Cloudflare, create `dibs.hivecommons.dev` as an A record pointing
   to `157.151.252.29`. Leave it **DNS only / grey cloud**.
2. **Let's Encrypt hold:** Wait until the certificate quota window has headroom,
   expected around **2026-09-10**. Do not trigger cert-manager re-issuance before
   then; an early 429 can push the retry window out further.
3. **Certificate SAN:** After the hold, confirm the SAN is not already present,
   then apply the JSON Patch in
   `src/deploy/dibs-domain-cutover/01-hive-wildcard-tls-certificate.yaml` so
   `hive-hub/hive-wildcard-tls` includes `dibs.hivecommons.dev` in `dnsNames`
   without replacing the rest of the Certificate:

   ```sh
   kubectl -n hive-hub get certificate hive-wildcard-tls -o jsonpath='{.spec.dnsNames}'
   kubectl -n hive-hub patch certificate hive-wildcard-tls --type=json --patch-file src/deploy/dibs-domain-cutover/01-hive-wildcard-tls-certificate.yaml
   ```
4. **Dual-host ingress:** After the Certificate is ready, review and apply
   `src/deploy/dibs-domain-cutover/02-dibs-ingress-dual-host.yaml` so the
   `dibs/dibs` Ingress serves both hosts while verification runs. The staged
   Ingress keeps `dibs.kubestellar.io` on its existing `hive-tls-hc` secret and
   relies on the ingress-nginx default `hive-hub/hive-wildcard-tls` certificate
   for `dibs.hivecommons.dev` after the SAN update is ready — which is the
   assumption step 0 exists to confirm, and the one that fails silently if it
   does not hold.
5. **Verify the new canonical host.** A `200` is necessary and not
   sufficient here — both failure modes this step exists to catch still answer
   `200`:

   ```sh
   # Fails outright on an invalid chain, rather than reporting a cheerful 200
   # served under ingress-nginx's self-signed default certificate.
   curl -sSI https://dibs.hivecommons.dev

   # Name the issuer and the SANs, so "there is a certificate" is not mistaken
   # for "there is the RIGHT certificate".
   openssl s_client -connect dibs.hivecommons.dev:443 \
     -servername dibs.hivecommons.dev </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer -subject -ext subjectAltName

   kubectl -n dibs get certificate,ingress
   ```

   Then the acceptance test that actually matters: **a browser already signed in
   to the hub must be recognized by `dibs` on the new host.** A signed-out `dibs`
   also renders fine and also returns `200`, so nothing above distinguishes a
   working SSO bridge from the broken one this cutover exists to repair — see
   [Moving a public hostname](hivecommons-migration.md#moving-a-public-hostname).
6. **Redirect the legacy host.** Once the new host is proven, remove
   `dibs.kubestellar.io` from the application Ingress **first**, then apply
   `src/deploy/dibs-domain-cutover/03-dibs-kubestellar-redirect.yaml`. Keep the
   old per-host TLS secret/certificate until the redirect has been verified.

   The order is not stylistic. Both Ingresses live in the `dibs` namespace and
   would claim the same host and path, and ingress-nginx resolves a duplicate
   host+path claim in favour of the **older** Ingress, logging a warning and
   nothing more. Apply the redirect while the application Ingress still lists
   the host and the redirect is simply inert: `curl` keeps returning `200`, the
   objects all look applied, and the obvious reading — "the redirect has not
   landed yet" — sends you to re-apply it rather than to the host it is losing
   to. `preflight.sh` check 4 reports this collision if it already exists.

   ```sh
   # Confirm exactly one Ingress claims the legacy host before applying.
   kubectl -n dibs get ingress \
     -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.rules[*].host}{"\n"}{end}'
   ```
7. **Verify the redirect, on a deep path.** The redirect annotation relies on
   `$request_uri` to carry the path and query across, and a redirect that drops
   them passes a `/`-only check while silently breaking every deep link into
   `dibs`:

   ```sh
   # Root: expect 308 to https://dibs.hivecommons.dev/
   curl -sI https://dibs.kubestellar.io | grep -Ei '^(HTTP|location)'

   # Deep path AND query: the Location must carry both across.
   curl -sI 'https://dibs.kubestellar.io/ideas/42?ref=x' | grep -Ei '^(HTTP|location)'

   kubectl -n dibs get certificate,ingress
   ```

   Confirm the status is `308`, the `Location` is
   `https://dibs.hivecommons.dev/ideas/42?ref=x` (not the bare host), and that
   the new host still serves successfully.

## The dual-host window is a verification window, not a resting state

Between steps 4 and 6 both hosts serve the application, which reads like a safe
place to pause. It is not, and the reason is the point of the whole issue: the
hub session cookie is scoped to `hivecommons.dev`, so a visitor arriving on
`dibs.kubestellar.io` is **not** signed in there and cannot be made to be —
a browser ignores a `Set-Cookie` whose `Domain` does not cover the host that
sent it, so no configuration scopes a `hivecommons.dev` session onto
`kubestellar.io` (see
[Moving a public hostname](hivecommons-migration.md#moving-a-public-hostname),
pinned by `src/pkg/hub/sibling_host_migration_test.go`).

That is today's already-broken behaviour rather than a regression this sequence
introduces — but it means the legacy host is knowingly serving a signed-out
experience for as long as the window is open, and only step 6 ends it. Keep the
window short, and do not treat "both hosts answer" as the finish line.

## Rollback

- If DNS fails before the Certificate step, remove or correct the Cloudflare A
  record and stop; no cluster rollback is needed.
- If certificate issuance fails, remove `dibs.hivecommons.dev` from the
  `hive-wildcard-tls` `dnsNames` list and wait for cert-manager to settle before
  retrying.
- If the dual-host application Ingress fails, restore the previous `dibs/dibs`
  Ingress with only `dibs.kubestellar.io` and keep the per-host `hive-tls-hc`
  secret in place.
- If the redirect fails, delete or revert the redirect Ingress and restore
  `dibs.kubestellar.io` on the application Ingress. Do not delete the old cert
  until the redirect has been observed working and no clients depend on direct
  serving from the legacy host.
