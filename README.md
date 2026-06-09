# myispsucksv6

IPv6 has been a standard for over 25 years. DHCPv6 prefix delegation has been an RFC since 2003. And yet here we are — major US carriers handing out a single `/64` via SLAAC and calling it a day, as if nobody behind their gateway might want to actually route packets. This tool exists because some of us refuse to NAT our way out of a problem that shouldn't exist.

`myispsucksv6` is an NDP proxy daemon with dynamic prefix tracking for home routers behind ISPs that assign IPv6 via SLAAC but don't offer DHCPv6 prefix delegation (T-Mobile Home Internet, AT&T Internet Air, Verizon 5G Home, etc.).

## The problem

Your ISP gives you a single `/64` on your WAN interface via SLAAC. You have a Linux router between the modem and your LAN. You want LAN clients to get real, routable IPv6 addresses — no NAT — but the ISP prefix changes over time and the prefix can't be delegated.

The existing tool for this is `ndppd`, which proxies NDP so your upstream gateway believes your LAN devices live on the WAN interface. It works, but it assumes a static prefix. When T-Mobile renumbers you, everything breaks until you manually reconfigure `ndppd`, your router's LAN address, and your RA daemon.

`myispsucksv6` fixes that.

## What it does

- Watches the upstream interface for global IPv6 prefix changes via netlink
- Automatically updates its NDP proxy rules when the prefix changes
- Assigns/updates the router's own address on the LAN interface (`<new-prefix>::1`)
- Sends Router Advertisements (with Prefix Information and RDNSS) on LAN interfaces — no radvd or dnsmasq RA config needed
- Fires optional hook scripts on prefix change (for nftables reload, etc.)

What it does **not** do: run DHCPv6, or touch your firewall.

## Quick start

### 1. Build and install

Requires Go 1.22+:

```bash
git clone https://github.com/bradleypeabody/myispsucksv6
cd myispsucksv6
mkdir -p bin
go build -o bin/myispsucksv6 ./cmd/myispsucksv6
install -m 755 bin/myispsucksv6 /usr/local/bin/myispsucksv6
```

### 2. Configure

```bash
cp examples/myispsucksv6.jsonc /etc/myispsucksv6.jsonc
```

Edit `/etc/myispsucksv6.jsonc` and set your interface names:

```jsonc
{
  "global": {
    "log_level": "info"
  },
  "upstream": [
    {
      "interface": "enp1s0",      // WAN interface — change this
      "debounce_seconds": 5,
      "proxy": [
        {
          "to_interface": "enp2s0", // LAN interface — change this
          "lan_host_suffix": "::1"
        }
      ]
    }
  ]
}
```

This tells `myispsucksv6` to:
- Watch `enp1s0` for a global unicast IPv6 prefix
- Proxy NDP from `enp1s0` to/from `enp2s0`
- Keep `<current-prefix>::1` on `enp2s0`
- Send Router Advertisements on `enp2s0` so LAN clients auto-configure via SLAAC

### 3. Install the systemd unit

```bash
cp examples/myispsucksv6.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now myispsucksv6
```

Check that it's running:

```bash
systemctl status myispsucksv6
journalctl -u myispsucksv6 -f
```

That's it. LAN clients should start receiving Router Advertisements and configuring IPv6 addresses within a few seconds.

## Troubleshooting

**LAN clients aren't getting IPv6 addresses.**

Check that the upstream interface has a global unicast address:

```bash
ip -6 addr show dev enp1s0 scope global
```

If it only shows `scope link`, your ISP hasn't assigned a prefix yet — check your WAN connection.

**NDP proxy isn't responding to Neighbor Solicitations.**

Check the daemon has the capabilities it needs:

```bash
journalctl -u myispsucksv6 | grep -i "socket\|capability\|permission\|error"
```

**Prefix keeps flapping.**

Increase `debounce_seconds` in your config (10–30 seconds is a reasonable range for cellular ISPs).

**Conflict with existing `ndppd`.**

Stop and disable `ndppd` before running `myispsucksv6`:

```bash
systemctl disable --now ndppd
```

## Further reading

See [DOC.md](DOC.md) for full configuration reference, hook scripts, advanced options, and architecture notes.

## Requirements

- Linux kernel 4.9+
- Go 1.22+ (to build from source)

## License

Apache-2.0. See [LICENSE](LICENSE).
