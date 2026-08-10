# ClankeR contributor relay

ClankeR lets a contributor lend their local AI CLI subscription to a hive. The relay connects to `/contribute`, receives one task at a time, runs the selected CLI in the contributor's environment, and reports completion/PR metadata back to the hive.

## Basic setup

From a checkout of this repository:

```bash
export HIVE_HUB=wss://hive.example.com/contribute
just contribute-setup claude
just contribute-hive
```

`compose-contributor.yaml`, `Dockerfile.contributor`, and the `just contribute-hive` recipe are the reference container path. Native mode is available through `just contribute-hive <backend> local` when a container runtime is not desired.

## Multi-hub subscription

A single relay can subscribe to multiple hives. Register with each hive first, then provide matching comma-separated lists:

```bash
export HIVE_HUB='wss://hive-a.example.com/contribute,wss://hive-b.example.com/contribute'
export HIVE_REGISTRATION_TOKEN='token-from-hive-a,token-from-hive-b'
just contribute-hive
```

The lists are positional: the first token belongs to the first hub, the second token belongs to the second hub, and so on. If the counts differ, the relay refuses to start rather than sending a token to the wrong hub.

The relay keeps a WebSocket and heartbeat for each subscribed hub, but shares one CLI/tmux session and works on only one task at a time. It rotates to another hub when the active hub has no assignable work. A task that is blocked on human action stays with its owning hub; the relay does not mix task state across hubs.

## Acting as a spoke agent role

Set `HIVE_AGENT_ROLE` to request a delegated role, or use the **Acting as** control in `/contribute` where available:

```bash
export HIVE_AGENT_ROLE=quality
just contribute-hive
```

The hive may override the request with an owner-assigned role. See [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md) for tier, grant, and allow-list requirements.

