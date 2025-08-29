# wait4tailscale

Basically a rich man's version of this poor man's bash script:

```bash
while ! tailscale status --json | jq --exit-status '.Self.Online' >/dev/null
  sleep 1
done
```

## Usage

Block until tailscale reaches an online or offline state. Returns immediately if its already in its desired state.

```
$ wait4tailscale --online
$ wait4tailscale --offline
```

Or watch a log of all changes

```
$ wait4tailscale --watch
2025-08-29 12:00:00 online
2025-08-29 12:10:00 offline
2025-08-29 12:20:00 online
```

Will automatically detect the default socket location, but otherwise a socket can be explicitly given.

```
$ wait4tailscale --socket /var/run/tailscale/tailscaled.sock
```

Another feature is syncing a systemd target to the current online state. This functions alot of like the built in `network-online.target`.

```
$ wait4tailscale systemd --target=tailscale-online.target
```
