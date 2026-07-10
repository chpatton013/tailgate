# Tailgate usage

Hands-on guide to reaching a private tailnet from your host through tailgate's proxy.

## Prerequisites

- Docker running (Docker Desktop provides the Linux VM the container runs in).
- For `bin/mint-authkey`: `python3` and the `aws` CLI configured with read access to
  the `headscale/admin-api-key` secret. (Or skip it and mint a key in the Headplane UI.)

## 1. Configure

```sh
cp .env.example .env
```

Get a pre-auth key, either:

```sh
./bin/mint-authkey --write-env          # mints via the Headscale API + AWS secret
```

or mint one in the [Headplane UI](https://headplane.example.com) and paste it into
`.env` as `TS_AUTHKEY=...`. Defaults in `.env` point at `https://headscale.example.com`,
hostname `tailgate-laptop`.

## 2. Start

```sh
./bin/tailgate up              # userspace mode; SOCKS5 on 127.0.0.1:1055, HTTP on :1056
./bin/tailgate logs            # watch it register; Ctrl-C when it's up
./bin/tailgate doctor          # checks container, registration, and proxy reachability
```

The node appears in Headplane. Only your registered nodes have MagicDNS names (e.g.
`myhost`, `exit-node`); names resolve as `<node>.ts.example.com`.

## 3. Reach the tailnet

### Terminal

```sh
# curl through the proxy — socks5h resolves names at the proxy, so MagicDNS works:
./bin/tailgate curl -sS -o /dev/null -w '%{http_code}\n' http://myhost.ts.example.com:<port>

# ssh through the proxy (pass any ssh flags straight through, e.g. -i for a key):
./bin/tailgate ssh -i ~/.ssh/id_ed25519 user@myhost.ts.example.com
```

> **macOS curl + TLS:** the system `curl` is LibreSSL and treats a server's
> `unrecognized_name` SNI warning as fatal (`error:...:tlsv1 unrecognized name`). If you
> hit that against an HTTPS service, use plain `http://`, add `-k` for a self-signed
> cert, or install OpenSSL-backed curl (`brew install curl`).

### Loopback-bound services (`tailgate forward`)

A service bound to `127.0.0.1` on a node (e.g. a gateway UI on `:8000`) is *not*
reachable over the tailnet directly. Forward it to the host instead. The host argument
is `[user@]host` and any extra flags pass straight through to `ssh`:

```sh
# via a ~/.ssh/config alias:
./bin/tailgate forward myhost 8000                 # then browse http://localhost:8000

# or fully self-contained, no ssh config needed:
./bin/tailgate forward user@myhost.ts.example.com 8000 -i ~/.ssh/id_ed25519

# pick a different local port (here 28789 -> remote 8000):
./bin/tailgate forward myhost 8000 28789 -i ~/.ssh/id_ed25519
```

Leave it running (Ctrl-C stops it). The tailnet-native alternative is to publish the
service from the node with `tailscale serve`, or rebind it to the node's tailnet IP.

### Your own tools

`./bin/tailgate proxy` prints ready-to-paste config. The essentials:

```sh
export ALL_PROXY=socks5h://127.0.0.1:1055      # curl, git, many CLIs
```

The `tailgate ssh`/`forward` wrappers inject the `ProxyCommand` themselves, so they need
no SSH config. A `Host` alias is optional convenience — it lets you shorten
`tailgate forward user@myhost.ts.example.com … -i …` to `tailgate forward myhost`:

```ssh-config
# ~/.ssh/config — per-host identity (no ProxyCommand needed for the wrapper).
Host myhost
    HostName myhost.ts.example.com
    User user
    IdentityFile ~/.ssh/id_ed25519
```

Add the block below *only* if you also want plain `ssh myhost` (outside the wrapper) to
work — it supplies the proxy when `tailgate` isn't in the loop:

```ssh-config
Host *.ts.example.com 100.64.*
    ProxyCommand nc -X 5 -x 127.0.0.1:1055 %h %p
```

> **DNS note:** the `socks5h` scheme and the `nc -X 5` ProxyCommand send the *hostname*
> to the proxy, so MagicDNS names resolve correctly without any host DNS changes. If a
> tool resolves DNS locally instead, give it the `100.64.x.y` address (`tailgate status`
> lists peers and IPs).

### Browser

- **Firefox** (recommended — per-profile proxy): Settings → Network Settings → Manual
  proxy → SOCKS Host `127.0.0.1`, Port `1055`, SOCKS v5, and tick **"Proxy DNS when using
  SOCKS v5"**. Use a dedicated profile so only tailnet browsing goes through the proxy.
- **Chrome**: launch a separate instance, e.g. `open -na "Google Chrome" --args
  --user-data-dir=/tmp/tailgate-chrome --proxy-server=socks5://127.0.0.1:1055`.

Then open a tailnet node's web UI, e.g. `http://myhost.ts.example.com:<port>`.

## Reaching subnet-routed hosts (kernel mode)

By default tailgate runs `tailscaled` in **userspace mode**: no TUN device, so it can
dial direct tailnet peers (`100.64.0.0/10`) but not hosts reachable only *through* a
subnet router (e.g. a peer behind an exit node advertising `10.0.0.0/16`).

Set `TS_MODE=kernel` in `.env` and add `--accept-routes` to `TS_UP_EXTRA_ARGS` to get a
real `tailscale0` interface with `NET_ADMIN` + `/dev/net/tun`:

```sh
# .env
TS_MODE=kernel
TS_UP_EXTRA_ARGS=--accept-routes
```

```sh
./bin/tailgate down     # remove the userspace container first — same container name
./bin/tailgate up       # starts the kernel-mode profile instead
```

This is **container-scoped**, not host-scoped: your machine never gets a route to
`10.0.0.0/16`, and doesn't need one. `tailscaled`'s own SOCKS5/HTTP proxy (still
published to host loopback exactly as in userspace mode) now dials through
`tailscaled`'s route table directly, so `tailgate curl`/`tailgate ssh`/`tailgate
forward` and any `socks5h://` client on the host reach subnet-routed hosts with zero
other host-side changes:

```sh
./bin/tailgate curl http://10.0.0.10:<port>
```

Docker Desktop grants `NET_ADMIN`/`/dev/net/tun` inside its own Linux VM, so this
doesn't create a host-level VPN interface or profile the way a native `tailscaled`
would — but it's a newer, less-exercised path than userspace mode. If it misbehaves,
switch back with `TS_MODE=userspace` (or delete the line — that's the default).

Verify it actually took effect (`doctor` alone won't tell you — it passes in both modes):

```sh
docker exec tailgate ip link show tailscale0        # real interface, not "does not exist"
docker exec tailgate ip route get 10.0.0.10          # "dev tailscale0", not "unreachable"
```

## Common operations

| Command | Does |
| --- | --- |
| `tailgate up` | start the client + proxy |
| `tailgate down` | stop + remove container (state volume persists) |
| `tailgate status` | `tailscale status` inside the container |
| `tailgate ip` | this node's tailnet IPs |
| `tailgate logs` | follow logs (watch registration) |
| `tailgate proxy` | print proxy addresses + config snippets |
| `tailgate doctor` | end-to-end health check |

## Troubleshooting

- **`TS_AUTHKEY` errors on `up`** — `.env` is missing or the key is empty/expired. Mint a
  fresh one (`./bin/mint-authkey --write-env`).
- **Not registering** — check `tailgate logs`. Confirm the container can reach
  `https://headscale.example.com` and that the key hasn't expired or been used up
  (single-use keys register one node; the state volume preserves identity across
  restarts, so you normally register only once).
- **`x509: certificate signed by unknown authority`** (on `fetch control key`) — your
  egress is behind a corporate SSL-decrypt proxy that re-signs HTTPS with a CA the
  container doesn't trust. Export your corporate root CA into `certs/` and restart:
  ```sh
  security find-certificate -a -p -c "<Org>" /Library/Keychains/System.keychain > certs/corp-ca.pem
  ./bin/tailgate down && ./bin/tailgate up
  ```
  The container mounts `certs/` at `/certs` (`SSL_CERT_DIR`) and trusts everything in it
  alongside the standard roots.
- **`AuthKey not found`** — the key's user was renamed/recreated server-side; mint a new
  key.
- **Proxy not reachable** — confirm the container is up with `tailgate doctor`.
- **MagicDNS name won't resolve** — the tool is resolving locally; use the `100.x` IP, or
  use `tailgate curl` / the `nc` ProxyCommand, which defer DNS to the proxy.
