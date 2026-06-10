# myispsucksv6 — full documentation

## Configuration reference

The config file is [JSONC](https://github.com/tailscale/hujson) (JSON with `//` comments and trailing commas). Default path: `/etc/myispsucksv6.jsonc`.

```jsonc
{
  "global": {
    "log_level": "info",           // debug | info | warn | error (default: info)
    "state_file": "/var/lib/myispsucksv6/state.json" // persists last-seen prefix across restarts
  },
  "upstream": [
    {
      "interface": "enp1s0",       // WAN interface to watch (required)
      "prefix_filter": "2000::/3", // only proxy prefixes matching this CIDR (default: 2000::/3)
      "debounce_seconds": 5,       // wait this long after last event before acting (default: 5)
      "proxy": [
        {
          "to_interface": "enp2s0",           // LAN interface (required)
          "lan_host_suffix": "::1",           // router gets <prefix>::<suffix> on LAN (default: ::1)

          // Router Advertisement settings (built-in RA is on by default)
          "disable_ra": false,                // set true to use radvd/dnsmasq for RA instead
          "ra_interval_seconds": 200,         // max unsolicited RA interval (default: 200)
          "ra_router_lifetime_seconds": 1800, // router lifetime in RA (default: 1800)
          "ra_mtu": 1500,                     // MTU option in RA (default: 1500)
          "ra_dns_servers": ["self"],         // RDNSS servers; "self" = router's own LAN address
          "ra_valid_lifetime": 86400,         // prefix valid lifetime in seconds (default: 86400)
          "ra_preferred_lifetime": 14400,     // prefix preferred lifetime in seconds (default: 14400)

          // NDP proxy settings
          "disable_ndp_proxy": false          // set true to disable the built-in NDP proxy
        }
      ]
    }
  ],
  "hooks": {
    "on_prefix_change_dir": "/etc/myispsucksv6/hooks.d" // directory of hook scripts
  }
}
```

## Hook scripts

Hook scripts run on every stable prefix change. Place executable files in the `on_prefix_change_dir` directory; they run in filename order with a 30-second timeout per script.

Environment variables passed to each hook:

| Variable | Example | Description |
|---|---|---|
| `ACTION` | `changed` | `added`, `changed`, or `removed` |
| `INTERFACE` | `enp1s0` | The upstream WAN interface |
| `NEW_PREFIX` | `2607:fb92:601:4570::/64` | The new prefix |
| `OLD_PREFIX` | `2607:fb92:601:3a20::/64` | The previous prefix (empty on `added`) |

Example: reload nftables when the prefix changes:

```bash
#!/bin/sh
# /etc/myispsucksv6/hooks.d/10-reload-nftables
nft -f /etc/nftables.conf
```

Example: reload dnsmasq (only needed if using dnsmasq for DHCPv4 with address ranges tied to the prefix):

```bash
#!/bin/sh
# /etc/myispsucksv6/hooks.d/20-reload-dnsmasq
systemctl reload dnsmasq
```

## dnsmasq setup

`myispsucksv6` handles Router Advertisements directly, so dnsmasq does **not** need `enable-ra` or any radvd configuration. dnsmasq is only needed if you want DHCPv4 or a local DNS resolver.

A minimal dnsmasq config for DHCPv4 + DNS resolver (no IPv6-specific settings needed):

```
# /etc/dnsmasq.d/router.conf
dhcp-range=192.168.1.100,192.168.1.200,12h
```

## Commands

```
myispsucksv6 [--config <path>] [--log-format text|json]
myispsucksv6 dump
myispsucksv6 test-hooks [--old-prefix <cidr>] [--new-prefix <cidr>]
```

- `dump` — print the currently tracked prefix and LAN addresses from the state file and exit
- `test-hooks` — run hook scripts with synthetic environment variables to verify they work

```bash
# Test hooks with a fake prefix change
myispsucksv6 test-hooks \
  --old-prefix 2607:fb92:601:3a20::/64 \
  --new-prefix 2607:fb92:601:4570::/64
```

## Disabling built-in RA

If you prefer to use radvd or dnsmasq for Router Advertisements, set `disable_ra: true` in the proxy config. You'll need to reload radvd/dnsmasq from a hook script when the prefix changes:

```jsonc
{
  "proxy": [
    {
      "to_interface": "enp2s0",
      "disable_ra": true
    }
  ]
}
```

With radvd, use `::/64` as the prefix so it advertises whatever `/64` is currently on the interface:

```
interface enp2s0 {
    AdvSendAdvert on;
    prefix ::/64 {
        AdvOnLink on;
        AdvAutonomous on;
    };
};
```

## IPv6 forwarding prerequisite

The daemon does not write any sysctls. You must enable IPv6 forwarding manually:

```bash
# /etc/sysctl.d/99-ipv6-forward.conf
net.ipv6.conf.all.forwarding = 1
```

Note: on Linux, enabling `net.ipv6.conf.all.forwarding` causes the kernel to ignore Router Advertisements received on all interfaces. If your WAN address comes from SLAAC (as it does with T-Mobile), you must also set:

```bash
net.ipv6.conf.<wan-interface>.accept_ra = 2
```

The `2` means "accept RA even when forwarding is enabled." `myispsucksv6` will log a warning at startup if it detects that forwarding is on but `accept_ra` is not set to `2` on the WAN interface.

### Why `noprefixroute` matters

When IPv6 forwarding is enabled, the kernel automatically marks SLAAC-assigned addresses with the `noprefixroute` flag. This suppresses the implicit on-link route that would otherwise be installed for the entire `/64` on the WAN interface. Without that suppression, the kernel would believe all addresses in your ISP's `/64` are directly reachable on the WAN segment — and would try to NDP-resolve your LAN clients out the WAN interface, bypassing the proxy entirely. The NDP proxy only works because no such route exists; `forwarding = 1` is what ensures it doesn't get created.

### Manually assigned WAN addresses

If your setup requires assigning a static IPv6 address to the WAN interface rather than relying on SLAAC, you must include the `noprefixroute` option yourself. With systemd-networkd, add this to your WAN `.network` file:

```ini
[Address]
Address = 2607:fb92:601:4570::1/64
NoPrefixRoute = yes
```

Without `NoPrefixRoute = yes`, systemd-networkd will install an on-link route for the `/64` on the WAN interface and the NDP proxy will not function correctly.

## How it works

```
netlink monitor ──addr events──▶ prefix manager (debounce)
                                        │
              ┌─────────────────────────┼──────────────────────┐
              ▼                         ▼                      ▼
        NDP proxy                 LAN addr manager        RA emitter
        · AF_PACKET on WAN        · assign <pfx>::1       · periodic RAs
        · receive NS              · delete old addr       · solicited RAs
        · probe LAN host                                  · RDNSS option
        · reply with NA
              │
              └──── hook runner
                    · exec scripts in hooks.d/
```

1. **netlink monitor** subscribes to kernel address events. When a new global unicast `/64` appears on the WAN interface, it forwards it to the prefix manager.

2. **prefix manager** debounces rapid events and publishes stable prefix changes.

3. **NDP proxy** opens `AF_PACKET` raw sockets on WAN and LAN. On a Neighbor Solicitation for an address in the proxied prefix, it probes the LAN host and — if alive — answers the WAN with a Neighbor Advertisement using the router's WAN MAC. This makes the ISP gateway believe LAN hosts live on the WAN segment.

4. **LAN address manager** assigns `<prefix>::<suffix>` on the LAN interface and removes the old prefix's address (new address added before old is removed).

5. **RA emitter** sends Router Advertisements with Prefix Information, RDNSS, and MTU options. Sends a deprecation RA (valid lifetime = 0) when the prefix changes so clients begin renumbering immediately.

6. **hook runner** executes scripts in `hooks.d/` in filename order after each stable prefix change.

## systemd unit details

The unit in `examples/myispsucksv6.service` uses `DynamicUser=yes` (no manual user creation) with only the two capabilities the daemon needs:

- `CAP_NET_RAW` — for `AF_PACKET` raw sockets (NDP proxy)
- `CAP_NET_ADMIN` — for netlink address management

The state file is written to `/var/lib/myispsucksv6/` (managed by systemd `StateDirectory=`).
