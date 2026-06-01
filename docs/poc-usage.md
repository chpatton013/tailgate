# Tailgate PoC — Tier 0 & Tier 1 usage

This is the hands-on guide for the Docker-based proof of concept. It gets you from
nothing to reaching private tailnet services (`*.ts.example.com`) from the host.
For the why and the full roadmap, see [`research-and-design.md`](research-and-design.md).

- **Tier 0** — Tailscale runs in TUN mode; you operate **from inside** the container.
  Purpose: validate the auth flow end to end.
- **Tier 1** — Tailscale runs in userspace mode and exposes a **SOCKS5 + HTTP proxy**
  on the host; `ssh`/`curl`/a browser reach the tailnet with no host VPN. This is the
  usable PoC.

## Prerequisites

- Docker Desktop running (it provides the Linux VM the container runs in).
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
`.env` as `TS_AUTHKEY=...`.

Defaults in `.env` already point at the headscale-infra tailnet
(`https://headscale.example.com`, hostname `tailgate-laptop`).

## 2. Tier 0 — validate connectivity from inside the container

```sh
./bin/tailgate up --tun
./bin/tailgate logs            # watch it register; Ctrl-C when you see it's up
./bin/tailgate status          # should list tailnet peers (e.g. exit-node)
```

The node should now appear in Headplane. Operate from inside the container, where
MagicDNS works. Only your registered nodes have MagicDNS names (e.g. `myhost`,
`exit-node`) — public ALB services like `app.example.com` are *not* tailnet
names.

```sh
docker compose exec tailscale sh
#  tailscale status
#  curl http://myhost.ts.example.com:<port>    # a web UI bound to the node
#  ssh user@myhost.ts.example.com
```

If this works, the auth flow is proven. Tear down before switching tiers:

```sh
./bin/tailgate down
```

## 3. Tier 1 — reach the tailnet from the host

```sh
./bin/tailgate up              # userspace mode; SOCKS5 on 127.0.0.1:1055, HTTP on :1056
./bin/tailgate doctor          # checks container, registration, and proxy reachability
```

### Terminal

```sh
# curl through the proxy — socks5h resolves names at the proxy, so MagicDNS works.
# Target a service bound to a node's tailnet IP (here, myhost's web UI):
./bin/tailgate curl -sS -o /dev/null -w '%{http_code}\n' http://myhost.ts.example.com:<port>

# ssh through the proxy (pass any ssh flags straight through, e.g. -i for a key):
./bin/tailgate ssh -i ~/.ssh/id_ed25519 user@myhost.ts.example.com
```

#### Loopback-bound services (`tailgate forward`)

A service bound to `127.0.0.1` on a node (e.g. myhost's gateway on `:8000`) is *not*
reachable over the tailnet directly — not via the proxy, and not even via Tier 2
routing. Forward it to the host instead. The host argument is `[user@]host` and any
extra flags pass straight through to `ssh`, so it behaves exactly like `ssh`:

```sh
# via a ~/.ssh/config alias:
./bin/tailgate forward myhost 8000                 # then browse http://localhost:8000

# or fully self-contained, no ssh config needed:
./bin/tailgate forward user@myhost.ts.example.com 8000 -i ~/.ssh/id_ed25519

# pick a different local port (here 28789 -> remote 8000):
./bin/tailgate forward myhost 8000 28789 -i ~/.ssh/id_ed25519
```

Leave it running (Ctrl-C stops it). The proper tailnet-native alternative is to publish
the service from the node with `tailscale serve`, or rebind it to the node's tailnet IP.

> **macOS curl + TLS:** the system `curl` is LibreSSL and treats a server's
> `unrecognized_name` SNI warning as fatal (`error:...:tlsv1 unrecognized name`). If you
> hit that against an HTTPS service, either use plain `http://`, add `-k` for a
> self-signed cert, or install OpenSSL-backed curl (`brew install curl`).

Or wire your own tools — `./bin/tailgate proxy` prints ready-to-paste config:

```sh
export ALL_PROXY=socks5h://127.0.0.1:1055      # curl, git, many CLIs
```

The `tailgate ssh`/`forward` wrappers inject the `ProxyCommand` themselves, so they need
no SSH config. A `Host` alias is purely optional convenience — it lets you shorten
`tailgate forward user@myhost.ts.example.com … -i …` to `tailgate forward myhost`:

```ssh-config
# ~/.ssh/config — per-host identity (no ProxyCommand needed for the wrapper).
Host myhost
    HostName myhost.ts.example.com
    User user
    IdentityFile ~/.ssh/id_ed25519
```

Add the block below *only* if you also want plain `ssh myhost` (outside the wrapper)
to work — it's what supplies the proxy when `tailgate` isn't in the loop:

```ssh-config
Host *.ts.example.com 100.64.*
    ProxyCommand nc -X 5 -x 127.0.0.1:1055 %h %p
```

> **DNS note:** the `socks5h` scheme and the `nc -X 5` ProxyCommand send the *hostname*
> to the proxy, so MagicDNS names resolve correctly without any host DNS changes. If a
> tool resolves DNS locally instead, give it the `100.64.x.y` address (`tailgate status`
> lists peers and IPs). System-wide MagicDNS on the host is a Tier 2 feature.

### Browser

Tailnet web UIs in a normal-ish browser:

- **Firefox** (recommended — per-profile proxy): Settings → Network Settings → Manual
  proxy → SOCKS Host `127.0.0.1`, Port `1055`, SOCKS v5, and tick
  **"Proxy DNS when using SOCKS v5"**. Use a dedicated profile so only tailnet browsing
  goes through the proxy.
- **Chrome**: launch a separate instance, e.g.
  `open -na "Google Chrome" --args --user-data-dir=/tmp/tailgate-chrome --proxy-server=socks5://127.0.0.1:1055`.

Then open your tailnet node's web UI, e.g. `http://myhost.ts.example.com:<port>`.

## Common operations

| Command | Does |
| --- | --- |
| `tailgate up` / `up --tun` | start Tier 1 / Tier 0 |
| `tailgate down` | stop + remove container (state volume persists) |
| `tailgate status` | `tailscale status` inside the container |
| `tailgate ip` | this node's tailnet IPs |
| `tailgate logs` | follow logs (watch registration) |
| `tailgate proxy` | print proxy addresses + config snippets |
| `tailgate doctor` | end-to-end health check |

## Troubleshooting

- **`TS_AUTHKEY` errors on `up`** — `.env` is missing or the key is empty/expired. Mint
  a fresh one (`./bin/mint-authkey --write-env`).
- **Not registering** — check `tailgate logs`. Confirm the container can reach
  `https://headscale.example.com` and that the key hasn't expired or been used up
  (single-use keys register exactly one node; the state volume preserves identity across
  restarts, so you normally only register once).
- **`AuthKey not found`** — the key's user was renamed/recreated server-side; mint a new
  key. (Same failure mode the headscale-infra preauthkey Lambda guards against.)
- **Proxy not reachable** — only exposed in Tier 1 (not `--tun`). Confirm with
  `tailgate doctor`.
- **MagicDNS name won't resolve** — the tool is resolving locally; use the `100.x` IP,
  or use `tailgate curl` / the `nc` ProxyCommand which defer DNS to the proxy.
