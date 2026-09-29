# Moving a Hive between hosts, same runtime

> **Stop the source before archiving, and keep it stopped while the target
> runs.** Two instances with the same `hive-id` can act on the same work.
> Preserve the stopped source until verification passes. To roll back, stop
> the target first, then restart the source; reconcile any work the target
> performed before reverting to the older source state.

Choose [rootless Podman](#podman-quadlet-rootless--rootless),
[rootful Podman](#podman-quadlet-rootful--rootful), or
[Docker Compose](#docker-compose--docker-compose-6522).
If registered with a hub, also follow
[hub cutover](move-hub-registered-cutover.md) for heartbeat and dashboard URL
changes. Arrange target DNS, firewall, reverse proxy/TLS and GitHub App callback
URLs before redirecting users; those host settings are not in the archives.

## Execution status

| Mode | Cross-host evidence |
| --- | --- |
| Rootless Podman, different subordinate-ID bases | **DOCUMENTED, NOT EXECUTED**. The rewrite was checked against repository scripts and units; no two-host run is available. |
| Rootful Podman | **DOCUMENTED, NOT EXECUTED**. No rootful host pair was available. |
| Docker Compose | **DOCUMENTED, NOT EXECUTED** as a two-host move. |

The [same-host backup evidence](backup-restore.md) is not a cross-host test.
The [execution record](#record-a-two-host-run) below lists the evidence still
needed for [#9663](https://github.com/hivecommons/hive/issues/9663).

## Podman Quadlet rootless → rootless

> **Before you start:** stop the source before exporting and never start both
> copies together. The volume archive carries `/data`, including `hive-id`,
> beads, runtime overlays, agent credentials and the backup encryption key.
> The config archive carries `$HOME/.config/hive`, including `hive.yaml`, `nginx.conf`,
> `hive.env` and `secrets/`. The usual `/secrets/gh-app-key.pem` travels in
> this config archive, not the volume. Match the source image digest before
> the first target start; upgrade deliberately after the move.

Use a fresh target with no existing `hive-data` volume or Hive units. Install
Podman with Quadlet and systemd, and a checkout of this repository on the
target. Run repository commands from its root. Use the same CPU architecture.
Review source unit overrides and reproduce needed custom mounts, ports and
configuration on the target; the config archive does not include unit overrides.
Archives contain credentials: use a private transfer directory and encrypted
transport, and escrow the backup encryption key outside both hosts.

1. **Source — record the baseline.** Pause new work and let active work finish.
   Use a private directory for the archives and save the outputs for comparison:

   ```bash
   umask 077
   CONF="$HOME/.config/hive"
   podman unshare cat /proc/self/uid_map /proc/self/gid_map
   podman exec hive cat /data/hive-id
   podman exec hive sh -c 'find /data/beads -type f | wc -l'
   podman exec hive curl -fsS http://127.0.0.1:3002/api/health
   podman logs hive 2>&1 | grep 'Config path pinned for the Go binary'
   # Set CONFIG_PATH to the effective --config path in the startup log.
   CONFIG_PATH=/etc/hive/hive.yaml   # replace if the entrypoint selected another path
   podman exec hive python3 -c 'import sys,yaml; c=yaml.safe_load(open(sys.argv[1])) or {}; print((c.get("github") or {}).get("key_file") or "No App key configured")' "$CONFIG_PATH"
   ```

   If the log has rotated, inspect the running Hive process arguments to find
   its effective `--config` path; do not assume the read-only seed is current.
   Set `APP_KEY` to that config value and record its digest (skip for a token-only
   hive). Resolve any environment-variable reference to its container value.
   If the path is outside `/data` and `/secrets`, archive its backing mount too.

   ```bash
   APP_KEY=/secrets/gh-app-key.pem   # replace with the actual configured path
   podman exec hive sha256sum "$APP_KEY"
   podman image inspect "$(podman inspect hive --format '{{.Image}}')" --format '{{json .RepoDigests}}'
   podman image inspect "$(podman inspect hive-gateway --format '{{.Image}}')" --format '{{json .RepoDigests}}'
   ```

   Save one pullable `repository@sha256:…` reference for each image. If there
   is no repository digest (a local build), transfer it with `podman save` /
   `podman load` in this mode and use a dedicated local tag with `Pull=never`.
   Do not substitute today's rolling `stable` tag.

2. **Source — stop, then archive.** Disable boot startup so a source reboot
   cannot start a duplicate during the move. Also suspend any operator-added
   update timers or other automation that could restart the source.

   ```bash
   systemctl --user disable --now hive-boot-gate.service
   systemctl --user stop hive-boot.target hive-gateway.service hive.service
   podman ps --format '{{.Names}}'   # neither hive nor hive-gateway may remain
   podman volume export hive-data -o hive-data.tar
   podman unshare tar czf hive-config.tar.gz -C "$(dirname "$CONF")" hive
   chmod 600 hive-data.tar hive-config.tar.gz
   ```

   Transfer both archives and the recorded digests/baseline to the target.
   Rootful archives are root-owned; use an authorized privileged transfer or
   give only the transferring operator access. Keep both archives mode 600.
   Keep the source volume and config intact.

3. **Target — check the host and install units without starting Hive.**

   ```bash
   CONF="$HOME/.config/hive"
   UNITS="$HOME/.config/containers/systemd"
   SYSTEMD_UNITS="$HOME/.config/systemd/user"
   env HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight.sh
   env HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-ids.sh
   podman unshare cat /proc/self/uid_map /proc/self/gid_map
   loginctl enable-linger "$(whoami)"
   mkdir -p "$UNITS" "$SYSTEMD_UNITS"
   cp src/deploy/quadlet/*.container src/deploy/quadlet/*.network src/deploy/quadlet/*.volume "$UNITS/"
   cp src/deploy/systemd/hive-boot.target src/deploy/systemd/hive-boot-gate.service "$SYSTEMD_UNITS/"
   systemctl --user daemon-reload
   ```

   Resolve failed preflight checks before continuing. Rootless needs delegated
   UID/GID ranges (at least 65536), local graphroot storage and a networking
   helper. Rootful uses host IDs directly: investigate unrelated accounts or
   groups using Hive's IDs before restoring. Do not enable the boot gate yet.

4. **Target — create the labelled volume and restore both archives.**
   Import only into the fresh empty volume created by its unit.

   ```bash
   systemctl --user start hive-data-volume.service
   podman volume import hive-data hive-data.tar
   mkdir -p "$(dirname "$CONF")"
   podman unshare tar xzf hive-config.tar.gz --no-same-owner -C "$(dirname "$CONF")"
   test -f "$CONF/hive.env"
   chmod 600 "$CONF/hive.env"
   podman unshare chown -R 0:1002 "$CONF/secrets"
   podman unshare find "$CONF/secrets" -type d -exec chmod 0750 {} +
   podman unshare find "$CONF/secrets" -type f -exec chmod 0640 {} +
   env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_LAYOUT=quadlet HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh
   ```

   `hive.env` holds tokens and must exist even if empty. If absent, restore it
   from the source; create an empty mode-600 file only if the source truly used
   no environment values. Preserve the dashboard token. Resolve host preflight
   failures (SELinux, config/secrets readability and port availability) before
   starting. Rootless secrets ownership must use the **target's** namespace.

5. **Target — pin both images before first start.** Set these variables to
   the source references recorded in step 1:

   ```bash
   HIVE_IMAGE='ghcr.io/hivecommons/hive@sha256:REPLACE_WITH_SOURCE_DIGEST'
   GATEWAY_IMAGE='docker.io/library/nginx@sha256:REPLACE_WITH_SOURCE_DIGEST'
   podman pull "$HIVE_IMAGE"
   podman pull "$GATEWAY_IMAGE"
   mkdir -p "$UNITS/hive.container.d" "$UNITS/hive-gateway.container.d"
   printf '[Container]\nImage=%s\n' "$HIVE_IMAGE" | tee "$UNITS/hive.container.d/90-move-image.conf" >/dev/null
   printf '[Container]\nImage=%s\n' "$GATEWAY_IMAGE" | tee "$UNITS/hive-gateway.container.d/90-move-image.conf" >/dev/null
   systemctl --user daemon-reload
   systemctl --user cat hive.service hive-gateway.service
   ```

   Confirm the generated commands use the recorded references, including any
   custom drop-ins. Leave auto-update off until verification is complete.

6. **Target — start and verify.** First confirm the source is still stopped.

   ```bash
   systemctl --user start hive.service hive-gateway.service
   systemctl --user is-active hive.service hive-gateway.service
   podman exec hive cat /data/hive-id
   APP_KEY=/secrets/gh-app-key.pem   # actual github.key_file from step 1
   podman exec hive sha256sum "$APP_KEY"   # skip for token-only installations
   podman exec hive sh -c 'find /data/beads -type f | wc -l'
   podman exec hive curl -fsS http://127.0.0.1:3002/api/health
   curl -fsS http://127.0.0.1:3001/api/health
   podman inspect hive hive-gateway --format '{{.Name}} {{.Image}}'
   ```

   Identity and key digest must match. Compare beads count and investigate
   unexpected losses; resumed work can add files. Confirm any source dashboard
   overlay is present and at least one agent backend works without unexpected
   re-authentication. Check the dashboard through its external URL too.
   Do not compare `hive-state.json` hashes: startup rewrites it.

7. **After successful verification — enable target boot startup.**

   ```bash
   systemctl --user enable hive-boot-gate.service
   ```

   Keep the source stopped for your rollback window. Only then, **on the source**,
   review the destructive teardown plan and remove its deployment:

   ```bash
   bin/hive-podman-teardown.sh plan
   bin/hive-podman-teardown.sh run --yes
   ```

   If verification fails, stop the target's services and boot target before
   starting the intact source again. Re-enable the source boot gate if reverting
   permanently. A failback loses target-only state unless you reconcile it first.

## Podman Quadlet rootful → rootful

> **Before you start:** stop the source before exporting and never start both
> copies together. The volume archive carries `/data`, including `hive-id`,
> beads, runtime overlays, agent credentials and the backup encryption key.
> The config archive carries `/etc/hive`, including `hive.yaml`, `nginx.conf`,
> `hive.env` and `secrets/`. The usual `/secrets/gh-app-key.pem` travels in
> this config archive, not the volume. Match the source image digest before
> the first target start; upgrade deliberately after the move.

Use a fresh target with no existing `hive-data` volume or Hive units. Install
Podman with Quadlet and systemd, and a checkout of this repository on the
target. Run repository commands from its root. Use the same CPU architecture.
Review source unit overrides and reproduce needed custom mounts, ports and
configuration on the target; the config archive does not include unit overrides.
Archives contain credentials: use a private transfer directory and encrypted
transport, and escrow the backup encryption key outside both hosts.

1. **Source — record the baseline.** Pause new work and let active work finish.
   Use a private directory for the archives and save the outputs for comparison:

   ```bash
   umask 077
   CONF="/etc/hive"
   sudo podman exec hive cat /data/hive-id
   sudo podman exec hive sh -c 'find /data/beads -type f | wc -l'
   sudo podman exec hive curl -fsS http://127.0.0.1:3002/api/health
   sudo podman logs hive 2>&1 | grep 'Config path pinned for the Go binary'
   # Set CONFIG_PATH to the effective --config path in the startup log.
   CONFIG_PATH=/etc/hive/hive.yaml   # replace if the entrypoint selected another path
   sudo podman exec hive python3 -c 'import sys,yaml; c=yaml.safe_load(open(sys.argv[1])) or {}; print((c.get("github") or {}).get("key_file") or "No App key configured")' "$CONFIG_PATH"
   ```

   If the log has rotated, inspect the running Hive process arguments to find
   its effective `--config` path; do not assume the read-only seed is current.
   Set `APP_KEY` to that config value and record its digest (skip for a token-only
   hive). Resolve any environment-variable reference to its container value.
   If the path is outside `/data` and `/secrets`, archive its backing mount too.

   ```bash
   APP_KEY=/secrets/gh-app-key.pem   # replace with the actual configured path
   sudo podman exec hive sha256sum "$APP_KEY"
   sudo podman image inspect "$(sudo podman inspect hive --format '{{.Image}}')" --format '{{json .RepoDigests}}'
   sudo podman image inspect "$(sudo podman inspect hive-gateway --format '{{.Image}}')" --format '{{json .RepoDigests}}'
   ```

   Save one pullable `repository@sha256:…` reference for each image. If there
   is no repository digest (a local build), transfer it with `podman save` /
   `podman load` in this mode and use a dedicated local tag with `Pull=never`.
   Do not substitute today's rolling `stable` tag.

2. **Source — stop, then archive.** Disable boot startup so a source reboot
   cannot start a duplicate during the move. Also suspend any operator-added
   update timers or other automation that could restart the source.

   ```bash
   sudo systemctl disable --now hive-boot-gate.service
   sudo systemctl stop hive-boot.target hive-gateway.service hive.service
   sudo podman ps --format '{{.Names}}'   # neither hive nor hive-gateway may remain
   sudo podman volume export hive-data -o hive-data.tar
   sudo tar czf hive-config.tar.gz -C "$(dirname "$CONF")" hive
   sudo chmod 600 hive-data.tar hive-config.tar.gz
   ```

   Transfer both archives and the recorded digests/baseline to the target.
   Rootful archives are root-owned; use an authorized privileged transfer or
   give only the transferring operator access. Keep both archives mode 600.
   Keep the source volume and config intact.

3. **Target — check the host and install units without starting Hive.**

   ```bash
   CONF="/etc/hive"
   UNITS="/etc/containers/systemd"
   SYSTEMD_UNITS="/etc/systemd/system"
   sudo env HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight.sh
   sudo env HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-ids.sh
   getent passwd 1001
   getent group 1000
   getent group 1002
   sudo mkdir -p "$UNITS" "$SYSTEMD_UNITS"
   sudo cp src/deploy/quadlet/*.container src/deploy/quadlet/*.network src/deploy/quadlet/*.volume "$UNITS/"
   sudo cp src/deploy/systemd/hive-boot.target src/deploy/systemd/hive-boot-gate.service "$SYSTEMD_UNITS/"
   sudo systemctl daemon-reload
   ```

   Resolve failed preflight checks before continuing. Rootless needs delegated
   UID/GID ranges (at least 65536), local graphroot storage and a networking
   helper. Rootful uses host IDs directly: investigate unrelated accounts or
   groups using Hive's IDs before restoring. Do not enable the boot gate yet.

4. **Target — create the labelled volume and restore both archives.**
   Import only into the fresh empty volume created by its unit.

   ```bash
   sudo systemctl start hive-data-volume.service
   sudo podman volume import hive-data hive-data.tar
   sudo mkdir -p "$(dirname "$CONF")"
   sudo tar xzf hive-config.tar.gz --no-same-owner -C "$(dirname "$CONF")"
   sudo test -f "$CONF/hive.env"
   sudo chmod 600 "$CONF/hive.env"
   sudo chown -R 0:1002 "$CONF/secrets"
   sudo find "$CONF/secrets" -type d -exec chmod 0750 {} +
   sudo find "$CONF/secrets" -type f -exec chmod 0640 {} +
   sudo env HIVE_DEPLOY_RUNTIME=podman HIVE_PODMAN_LAYOUT=quadlet HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh
   ```

   `hive.env` holds tokens and must exist even if empty. If absent, restore it
   from the source; create an empty mode-600 file only if the source truly used
   no environment values. Preserve the dashboard token. Resolve host preflight
   failures (SELinux, config/secrets readability and port availability) before
   starting. Rootless secrets ownership must use the **target's** namespace.

5. **Target — pin both images before first start.** Set these variables to
   the source references recorded in step 1:

   ```bash
   HIVE_IMAGE='ghcr.io/hivecommons/hive@sha256:REPLACE_WITH_SOURCE_DIGEST'
   GATEWAY_IMAGE='docker.io/library/nginx@sha256:REPLACE_WITH_SOURCE_DIGEST'
   sudo podman pull "$HIVE_IMAGE"
   sudo podman pull "$GATEWAY_IMAGE"
   sudo mkdir -p "$UNITS/hive.container.d" "$UNITS/hive-gateway.container.d"
   printf '[Container]\nImage=%s\n' "$HIVE_IMAGE" | sudo tee "$UNITS/hive.container.d/90-move-image.conf" >/dev/null
   printf '[Container]\nImage=%s\n' "$GATEWAY_IMAGE" | sudo tee "$UNITS/hive-gateway.container.d/90-move-image.conf" >/dev/null
   sudo systemctl daemon-reload
   sudo systemctl cat hive.service hive-gateway.service
   ```

   Confirm the generated commands use the recorded references, including any
   custom drop-ins. Leave auto-update off until verification is complete.

6. **Target — start and verify.** First confirm the source is still stopped.

   ```bash
   sudo systemctl start hive.service hive-gateway.service
   sudo systemctl is-active hive.service hive-gateway.service
   sudo podman exec hive cat /data/hive-id
   APP_KEY=/secrets/gh-app-key.pem   # actual github.key_file from step 1
   sudo podman exec hive sha256sum "$APP_KEY"   # skip for token-only installations
   sudo podman exec hive sh -c 'find /data/beads -type f | wc -l'
   sudo podman exec hive curl -fsS http://127.0.0.1:3002/api/health
   curl -fsS http://127.0.0.1:3001/api/health
   sudo podman inspect hive hive-gateway --format '{{.Name}} {{.Image}}'
   ```

   Identity and key digest must match. Compare beads count and investigate
   unexpected losses; resumed work can add files. Confirm any source dashboard
   overlay is present and at least one agent backend works without unexpected
   re-authentication. Check the dashboard through its external URL too.
   Do not compare `hive-state.json` hashes: startup rewrites it.

7. **After successful verification — enable target boot startup.**

   ```bash
   sudo systemctl enable hive-boot-gate.service
   ```

   Keep the source stopped for your rollback window. Only then, **on the source**,
   review the destructive teardown plan and remove its deployment:

   ```bash
   sudo bin/hive-podman-teardown.sh plan
   sudo bin/hive-podman-teardown.sh run --yes
   ```

   If verification fails, stop the target's services and boot target before
   starting the intact source again. Re-enable the source boot gate if reverting
   permanently. A failback loses target-only state unless you reconcile it first.

## Why these steps

### What moves

`hive-data` carries all of `/data`: identity, state, beads, dashboard/runtime
config overlays, `/data/home` backend credentials, and
`/data/secrets/backup_encryption_key`. Do not exclude `home/` on a host move.
The config archive carries the host bind mounts and `hive.env`. The App key
belongs to whichever archive covers the running config's `github.key_file`;
the standard Quadlet `/secrets` path is in the config archive.

The [entrypoint](../deploy/entrypoint.sh) restores runtime config through
`HIVE_CONFIG_RUNTIME`, repairs ownership through `hive_reown_path_if_needed`,
and rebuilds links under the “Create beads symlinks” comment. Those startup
operations are why verification includes credentials and overlays, not just
an unchanged identity file.

### Rootless ownership and SELinux

For the default rootless map, container UID 0 maps to the invoking user's
own UID; every other container UID `N` maps to `subuid_base + N - 1`.
Thus UID 1001 with base 524288 maps to **525288**, and with base 100000 to
**101000**. GIDs follow the analogous subordinate-GID map. Read the actual
maps with `podman unshare cat /proc/self/uid_map /proc/self/gid_map`; custom
user namespaces need a separately reviewed procedure.

`podman volume export/import` stores container IDs and translates them through
each host's namespace. A plain host-shell tar of rootless volume storage would
record the wrong host IDs. The config export also uses `podman unshare` to read
restricted secrets; extraction drops source ownership and explicitly restores
the target's `0:1002` ownership and group readability on secrets.

The [volume unit](../deploy/quadlet/hive-data.volume) supplies the ownership
labels used by teardown. The [container unit](../deploy/quadlet/hive.container)
mounts the named volume without `:Z`, but mounts `%E/hive/secrets` with `:ro,Z`.
Do not transplant source SELinux MCS categories; let Podman label target mounts.

## Record a two-host run

To change a mode's status to **OBSERVED**, execute its complete procedure on
actual source and target hosts and link a sanitized run record here. For the
rootless case the subordinate-ID bases must differ. Record:

- Date, repository revision, OS, Podman/systemd versions, architecture and
  root mode on both hosts; actual UID/GID maps for rootless.
- Source and target image digests, exact commands, exit codes and any deviations.
- Before/after `hive-id`, App key SHA-256 (never key contents), beads file count,
  service state and internal/gateway health responses.
- Successful backend authentication and boot persistence, source-stop timing,
  target-start timing and whether rollback/teardown was exercised.

Do not publish archives, tokens, private keys or full config. No execution
record is attached yet; the two-host acceptance run remains outstanding.

## Docker Compose → Docker Compose (#6522)

### What must move, cited to the deployment files

The Compose deployment persists three things
([backup-restore.md](backup-restore.md#durable-state)):

- the named volume `hive-data` (prefixed `src_hive-data` under the default
  project name — [`src/docker-compose.yaml`](../docker-compose.yaml)),
  mounted at `/data`, holding `hive-id`, `hive-state.json`,
  ``beads/`, `/data/home/*` (agent backend credentials —
  see “What moves” below), and the runtime config overlay
  `hive.yaml.runtime` / `hive.yaml.dashboard` restored over `/etc/hive/hive.yaml`
  at every boot ([`src/deploy/entrypoint.sh`](../deploy/entrypoint.sh),
  [`src/deploy/entrypoint.sh`](../deploy/entrypoint.sh));
- the host directory `./secrets`, bind-mounted read-only at `/secrets`
  ([`src/docker-compose.yaml`](../docker-compose.yaml));
- the host file `./hive.yaml`, bind-mounted at `/etc/hive/hive.yaml`
  ([`src/docker-compose.yaml`](../docker-compose.yaml)).

`/data/hive.yaml.dashboard` (the dashboard save overlay) lives inside the
volume already, so archiving `hive-data` carries it — no separate step needed,
unlike the config-file case in the Podman sections below where `hive.yaml`
lives outside the volume too.

### Procedure

1. On the **source**, confirm current identity for later comparison:
   ```bash
   docker compose -f src/docker-compose.yaml exec hive cat /data/hive-id
   # Set APP_KEY to github.key_file in the running config (usually /secrets/gh-app-key.pem).
   APP_KEY=/secrets/gh-app-key.pem
   docker compose -f src/docker-compose.yaml exec hive sha256sum "$APP_KEY"
   ```
2. **Stop the source before archiving.** `docker compose stop` (not `down -v`)
   leaves the volume in place while quiescing writes to `hive-state.json` and
   `beads/`:
   ```bash
   docker compose -f src/docker-compose.yaml stop
   ```
3. Archive the volume and the two host paths, per
   [backup-restore.md](backup-restore.md#host-level-backup-pattern):
   ```bash
   docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine \
     tar czf /backup/hive-data-$(date +%F).tar.gz -C /data .
   tar czf hive-config-$(date +%F).tar.gz ./src/hive.yaml ./src/secrets
   ```
4. Copy both archives to the target host (`scp`, or your usual transfer
   mechanism — out of scope here).
5. On the **target**, with the repo checked out at the same relative layout:
   ```bash
   docker volume create src_hive-data
   docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine \
     tar xzf /backup/hive-data-<date>.tar.gz -C /data
   tar xzf hive-config-<date>.tar.gz   # restores ./src/hive.yaml and ./src/secrets
   docker compose -f src/docker-compose.yaml up -d
   ```
6. **Verify before destroying the source** (compare identity, key digest and health with the source).
7. Only after verification passes, tear down the source:
   ```bash
   docker compose -f src/docker-compose.yaml down -v
   ```

## References

- [Backup and restore](backup-restore.md)
- [Podman standalone install](podman-standalone-quadlet.md)
- [ID, storage and networking preflight](podman-preflight-ids.md)
- [SELinux, config and ports preflight](podman-preflight-host.md)
- Original move work: [#6521](https://github.com/hivecommons/hive/issues/6521),
  [#6523](https://github.com/hivecommons/hive/issues/6523),
  [#6524](https://github.com/hivecommons/hive/issues/6524).
