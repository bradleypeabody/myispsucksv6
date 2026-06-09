# Scoping: `myispsucksv6` — NDP Proxy Daemon with Dynamic Prefix Tracking

## Problem statement

Home network setup:
- T-Mobile Home Internet (5G fixed wireless) as upstream
- T-Mobile provides IPv6 via SLAAC on the WAN interface only — a `/64` from a dynamic prefix that changes over time
- **No DHCPv6 prefix delegation** — T-Mobile (and similar 5G/cellular ISPs) don't support it
- A Linux router (Debian + systemd-networkd + nftables + dnsmasq) sits between the modem and the LAN

Goal: give LAN clients real, routable IPv6 addresses from the upstream-assigned `/64`, without NAT, automatically adapting when the ISP renumbers the prefix.

This is a common problem for anyone behind 5G/cellular fixed wireless (T-Mobile Home Internet, AT&T Internet Air, Verizon 5G Home) or any ISP that runs IPv6 but won't delegate prefixes. Hence the name.

## The architectural answer

NDP proxying (RFC 4389). The router pretends to be each LAN device at the neighbor-discovery layer on WAN. The upstream gateway sees neighbor advertisements for all LAN devices' addresses coming from the router's MAC, and routes traffic to those addresses to the router, which forwards to LAN. LAN clients SLAAC into the upstream-assigned `/64` directly. No NAT, no rewriting, no third-party tunnel.

The existing tool is `ndppd` (DanielAdolfsson/ndppd). Its gap: assumes the proxied prefix is statically configured. When the upstream prefix changes, every piece of the stack needs to be reconfigured (router LAN address, RA prefix in dnsmasq/radvd, the NDP proxy rule itself).

## Project: `myispsucksv6`

A daemon that observes the upstream IPv6 prefix via the kernel, automatically reconfigures its NDP-proxy rule, manages the router's LAN-side address, and emits hook events so the rest of the stack (dnsmasq/radvd/firewall) can react.

### Decisions

| Decision | Choice |
|---|---|
| Name | `myispsucksv6` |
| License | Apache-2.0 |
| Language | Pure Go, no CGO |
| Config format | TOML (via `github.com/pelletier/go-toml/v2`) |
| Target platforms | Linux only (Linux build tags throughout) |
| Privilege model | Caps via systemd ambient capabilities, default `DynamicUser=yes` |
| Scope | NDP proxy + prefix tracking + LAN address management. RA/DHCPv6/firewall integration via hook scripts. Do not reimplement RA. |

### Why pure Go (not C++, not Rust, not CGO)

- **C++ (patch ndppd)**: smallest delta but worst foundation. Custom smart pointers, no tests, hand-rolled config parser, effectively unmaintained upstream — your fork would be on its own.
- **Rust**: best technical foundation but biggest unfamiliarity tax for someone with deep Go experience.
- **Go with CGO**: unnecessary. Nothing this daemon needs requires C interop.
- **Pure Go**: matches existing expertise, has all the libraries needed, single static binary, trivial cross-compilation, clean systemd integration.

The Go ecosystem has the entire surface area covered:
- `github.com/mdlayher/ndp` — NDP packet parsing/serialization
- `golang.org/x/net/ipv6` — raw IPv6 sockets
- `github.com/jsimonetti/rtnetlink` or `github.com/vishvananda/netlink` — kernel netlink subscription for address/route events
- `golang.org/x/sys/unix` — low-level syscalls (capabilities, setsockopt, etc.)
- `github.com/pelletier/go-toml/v2` — TOML config

### Why not reimplement Router Advertisements

dnsmasq and radvd are mature, well-tested, and configurable. Reimplementing RA in `myispsucksv6` would mean re-solving problems they already solve (DHCPv6 options, DNS server announcement, MTU, route announcements, etc.). Instead: when the prefix changes and the router's LAN address gets updated, fire a hook that reloads dnsmasq/radvd. They pick up the new prefix automatically from the LAN interface.

Documented recommendation for dnsmasq users (works out of the box once the LAN interface has the right prefix):

```
# /etc/dnsmasq.d/router.conf
enable-ra
dhcp-range=::,constructor:enp2s0,ra-stateless,ra-names,12h
```

For radvd users:

```
# /etc/radvd.conf
interface enp2s0 {
    AdvSendAdvert on;
    prefix ::/64 {
        AdvOnLink on;
        AdvAutonomous on;
        AdvRouterAddr on;
    };
};
```

The `::/64` prefix tells radvd to advertise whatever /64 is currently assigned to the interface.

## Architecture

```
                                  +---------------------+
                                  |   config (TOML)     |
                                  +---------------------+
                                            |
                                            v
+-------------------+              +---------------------+
| netlink monitor   |--addr/rt---->|   prefix manager    |
| (rtnetlink)       |   events     | (current state per  |
+-------------------+              |  upstream iface,    |
                                   |  with debounce)     |
                                   +----------+----------+
                                              | publish prefix changes
                +-----------------------------+-----------------------------+
                |                             |                             |
                v                             v                             v
   +-----------------------+   +-----------------------+   +-----------------------+
   | NDP listener (WAN)    |   | LAN address manager   |   | hook runner           |
   | - receive NS          |   | - assign router IP    |   | - exec scripts in     |
   | - match rules         |   |   on LAN ifaces       |   |   hooks.d/ with env   |
   | - probe LAN if needed |   | - delete old IPs      |   |   vars (NEW_PREFIX,   |
   | - send NA             |   +-----------------------+   |   OLD_PREFIX, IFACE)  |
   +-----------------------+                               +-----------------------+
```

Components and rough sizes:

| Component | LoC est. | Effort |
|---|---|---|
| Config parsing (TOML) | ~50 | 1 hr |
| Netlink monitor | ~150 | 3 hr |
| Prefix manager (with debounce) | ~200 | 4 hr |
| NDP listener (using mdlayher/ndp) | ~250 | 6 hr |
| LAN address management | ~100 | 2 hr |
| Hook runner | ~50 | 1 hr |
| Main + lifecycle + signal handling | ~100 | 2 hr |
| systemd unit | trivial | 15 min |
| Tests (unit + integration) | ~400 | 6 hr |
| Documentation (README, ARCHITECTURE, CONFIG, DEPLOYMENT) | n/a | 3 hr |
| **Total** | **~1300 LoC** | **~30 hr** |

A weekend gets a working prototype. Another weekend covers tests, packaging, and docs.

## Config example

```toml
# /etc/myispsucksv6.toml

[global]
log_level = "info"

# Observe an upstream interface; proxy any global IPv6 prefix that appears on it.
[[upstream]]
interface = "enp1s0"
# Optional: restrict which prefixes we'll proxy. Default: all global unicast (2000::/3).
# Excludes link-local (fe80::/10) and ULA (fd00::/8) automatically.
# prefix_filter = "2000::/3"

# Debounce: wait this long after a prefix-change event before reconfiguring,
# in case the upstream is flapping briefly.
debounce_seconds = 5

# For each upstream, configure one or more downstream interfaces to proxy to.
[[upstream.proxy]]
to_interface = "enp2s0"
# Assign the router's address on this LAN interface using this subnet ID.
# Result: <upstream-prefix>::<subnet_id>:1 (e.g., 2607:fb92:601:4570::1).
# Use 1 for /64 prefixes. Higher IDs only make sense if the upstream gives us larger.
lan_router_subnet_id = 1

[hooks]
# Directory of executable scripts to run on prefix changes.
# Each script receives env: NEW_PREFIX, OLD_PREFIX, INTERFACE, ACTION (changed|added|removed)
on_prefix_change_dir = "/etc/myispsucksv6/hooks.d"
```

Example hook to reload dnsmasq:

```bash
#!/bin/sh
# /etc/myispsucksv6/hooks.d/10-reload-dnsmasq
logger -t myispsucksv6 "Prefix changed on $INTERFACE: $OLD_PREFIX -> $NEW_PREFIX, reloading dnsmasq"
systemctl reload dnsmasq
```

## systemd integration

Three documented service file options.

### Option 1 (default, recommended): DynamicUser

Works on systemd 235+ (Debian 10+, Ubuntu 18.04+, RHEL 8+). systemd allocates a transient isolated UID/GID for the service, no system user setup needed:

```ini
[Unit]
Description=myispsucksv6 — IPv6 NDP proxy with dynamic prefix tracking
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/myispsucksv6
Restart=on-failure
RestartSec=5s

# Privilege model
DynamicUser=yes
AmbientCapabilities=CAP_NET_RAW CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_RAW CAP_NET_ADMIN
NoNewPrivileges=yes

# Hardening
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
SystemCallArchitectures=native
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

### Option 2: Named system user

For people who want a stable named user (log filtering, file ownership, etc.). Create the user once at install time:

```bash
useradd --system --shell /usr/sbin/nologin --no-create-home myispsucksv6
```

Then change the unit:

```ini
User=myispsucksv6
Group=myispsucksv6
# (remove DynamicUser=yes)
```

### Option 3: Root

Simplest, least isolated. Remove `User=`, `DynamicUser=`, `AmbientCapabilities=` lines. Document as the fallback for users who can't be bothered.

### Why not `User=nobody`?

Two reasons:
1. **Cross-distro inconsistency**: Debian/Ubuntu uses `nobody:nogroup` (group is `nogroup`), while RHEL/Fedora uses `nobody:nobody`. Specifying `Group=nobody` breaks on Debian.
2. **Shared-user anti-pattern**: `nobody` is conventionally the user for services with no privileges that share nothing. Multiple services running as `nobody` could in principle access each other's process state. Per-service isolation is better, and `DynamicUser=yes` gives it for free.

`DynamicUser=yes` is the modern correct answer to "I want a low-privilege isolated user without setup."

## Repository structure

```
myispsucksv6/
├── cmd/myispsucksv6/main.go    # entry point
├── internal/
│   ├── config/                 # TOML parsing
│   ├── netlink/                # rtnetlink event subscription
│   ├── prefix/                 # prefix tracking + debounce
│   ├── ndp/                    # NDP listener + responder (uses mdlayher/ndp)
│   ├── lanaddr/                # LAN-side address management
│   └── hooks/                  # hook script invocation
├── examples/
│   ├── myispsucksv6.toml       # example config
│   └── hooks.d/
│       ├── 10-reload-dnsmasq.sh
│       ├── 11-reload-radvd.sh
│       └── 20-notify-firewall.sh
├── systemd/
│   ├── myispsucksv6.service    # DynamicUser variant (default)
│   ├── myispsucksv6-user.service     # named-user variant
│   └── myispsucksv6-root.service     # root variant
├── docs/
│   ├── ARCHITECTURE.md
│   ├── CONFIG.md
│   ├── DEPLOYMENT.md
│   └── DEBIAN-EXAMPLE.md       # full end-to-end on Debian+dnsmasq
├── go.mod
├── go.sum
├── README.md
├── LICENSE                     # Apache-2.0
└── .github/workflows/          # CI: build, test, lint, release binaries
```

## Risks and unknowns

1. **Multi-address devices**: LAN clients have multiple IPv6 addresses (SLAAC, privacy, temporary). The reactive proxy model handles this naturally — listen for NS, check if target is on LAN, respond. No per-address state needed.

2. **Race on prefix change**: when the upstream prefix changes, there's a window where:
   - The router has the new prefix on WAN
   - dnsmasq hasn't yet reloaded
   - LAN clients still have old-prefix SLAAC addresses
   - Old-prefix addresses get sent to the upstream but the upstream now routes the new prefix
   
   Resolved by: the LAN address manager assigns the new prefix on the LAN interface first, then triggers the hook. dnsmasq picks up the new prefix and emits an RA with valid_lifetime=0 on the old prefix (RFC 4862 §5.5.3) — clients deprecate the old addresses quickly. The brief outage during transition is unavoidable but small.

3. **MAC address scaling**: the router will be proxying NDP for many addresses behind a single MAC. The upstream gateway shouldn't care (this is what proxy NDP is for) but some ISPs might apply per-MAC limits we don't know about. T-Mobile-specific testing needed.

4. **IPv6 address collisions**: if a LAN client picks a privacy address that collides with the router's `prefix::1`, weirdness ensues. Mitigation: use a subnet ID outside SLAAC's normal output. `::1` is in practice safe because SLAAC EUI-64 addresses always have the `ff:fe` bits set in the middle of the IID. Document the convention.

5. **CAP_NET_ADMIN scope**: needed for netlink subscription and address manipulation. CAP_NET_RAW for the raw ICMPv6 socket. Both are routine for network daemons.

## Open implementation questions (for the build phase)

- What's the right netlink library — `vishvananda/netlink` (more popular, slightly older API) vs `jsimonetti/rtnetlink` (lower-level, cleaner). Probably the former for ergonomics.
- Should the daemon write a state file (e.g., `/var/lib/myispsucksv6/state.json`) so it remembers the last-seen prefix across restarts? Useful for the hook on first-run-after-restart to know what changed. Probably yes, small win.
- CLI surface: `myispsucksv6` (run as daemon, foreground) is the main one. Also useful: `myispsucksv6 dump` to show current state, `myispsucksv6 test-hooks` to invoke hook scripts with synthetic env vars. Keep minimal.
- Logging: structured (slog) or text? slog with text handler by default, JSON handler optional. journald handles either.

## Build-first vs README-first

Recommend writing the README and `myispsucksv6 --help` first, before any production code. The README forces the user-facing contract to be concrete:
- What does the user have to configure?
- What's a working end-to-end example?
- What does the troubleshooting section say when things break?

If those are clear, the code is mostly mechanical translation. If they're fuzzy, the code accretes complexity. README-first is the cheapest design pass.
