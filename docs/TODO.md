# Tailgate — status & possible future work

**The userspace proxy is the finished product.** It's in daily use and there's no
outstanding work required to ship it — see the [README](../README.md) and
[`usage.md`](usage.md).

## Archived: the transparent-gateway exploration

The transparent L3-routing + host-wide-MagicDNS gateway VM (formerly `tier2/`) was
**removed**. It was functionally validated but its macOS substrate (QEMU+HVF) suffers an
unresolved guest hard-halt, and the whole approach is inherently macOS-specific (Apple
`vmnet` + `/etc/resolver`), which conflicts with keeping the project portable. The design,
the hard-won implementation gotchas, and the reasons it was dropped are preserved in
[`research-and-design.md` §10](research-and-design.md). Reviving it would mean re-platforming
onto a Mac-native VM (Apple Virtualization.framework) — a portability tradeoff that was
deliberately declined.

## Optional polish (not planned)

- A proxied browser-launcher (`tailgate browser`) and/or auto-starting the container on
  login. Deprioritized — the proxy + `./bin/tailgate proxy` snippets cover this today.
- Reverse-DNS / `tailscale serve` helpers for loopback-bound node services (currently handled
  by `tailgate forward`).
