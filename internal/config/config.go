package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"

	"github.com/tailscale/hujson"
)

type Config struct {
	Global   GlobalConfig     `json:"global"`
	Upstream []UpstreamConfig `json:"upstream"`
	Hooks    HooksConfig      `json:"hooks"`
}

type GlobalConfig struct {
	LogLevel  string `json:"log_level"`  // debug|info|warn|error
	StateFile string `json:"state_file"` // persists last-seen prefix across restarts
}

type UpstreamConfig struct {
	Interface       string        `json:"interface"`        // WAN interface to watch
	PrefixFilter    string        `json:"prefix_filter"`    // CIDR; only proxy prefixes within this range
	DebounceSeconds int           `json:"debounce_seconds"` // wait this long after a change before acting
	Proxy           []ProxyConfig `json:"proxy"`
}

type ProxyConfig struct {
	ToInterface         string   `json:"to_interface"`                // LAN interface to proxy to
	LANHostSuffix       string   `json:"lan_host_suffix"`             // router gets <prefix> | <suffix>, e.g. "::1"
	DisableNDPProxy     bool     `json:"disable_ndp_proxy"`           // set true to suppress built-in NDP proxy (use ndppd instead)
	DisableRA           bool     `json:"disable_ra"`                  // set true to suppress built-in RA (use radvd/hooks instead)
	RAIntervalSeconds   int      `json:"ra_interval_seconds"`         // max unsolicited RA interval (default 200)
	RARouterLifetime    int      `json:"ra_router_lifetime_seconds"`  // router lifetime in RA (default 1800)
	RAMTU               int      `json:"ra_mtu"`                      // MTU option in RA (default 1500)
	RADNSServers        []string `json:"ra_dns_servers"`              // RDNSS servers; "self" = router's LAN addr (default ["self"])
	RAValidLifetime     int      `json:"ra_valid_lifetime"`           // prefix valid lifetime in seconds (default 86400)
	RAPreferredLifetime int      `json:"ra_preferred_lifetime"`       // prefix preferred lifetime in seconds (default 14400)
}

type HooksConfig struct {
	OnPrefixChangeDir string `json:"on_prefix_change_dir"` // directory of executable hook scripts
}

// Load reads, parses, applies defaults to, and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	std, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("parsing JSONC: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(std, &cfg); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Global.LogLevel == "" {
		cfg.Global.LogLevel = "info"
	}
	if cfg.Global.StateFile == "" {
		cfg.Global.StateFile = "/var/lib/myispsucksv6/state.json"
	}
	for i := range cfg.Upstream {
		u := &cfg.Upstream[i]
		if u.PrefixFilter == "" {
			u.PrefixFilter = "2000::/3"
		}
		if u.DebounceSeconds == 0 {
			u.DebounceSeconds = 5
		}
		for j := range u.Proxy {
			if u.Proxy[j].LANHostSuffix == "" {
				u.Proxy[j].LANHostSuffix = "::1"
			}
		}
	}
}

func validate(cfg *Config) error {
	switch cfg.Global.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("global.log_level must be debug/info/warn/error, got %q", cfg.Global.LogLevel)
	}
	for i, u := range cfg.Upstream {
		if u.Interface == "" {
			return fmt.Errorf("upstream[%d]: interface is required", i)
		}
		if _, err := netip.ParsePrefix(u.PrefixFilter); err != nil {
			return fmt.Errorf("upstream[%d].prefix_filter: %w", i, err)
		}
		if u.DebounceSeconds < 0 {
			return fmt.Errorf("upstream[%d].debounce_seconds must be >= 0", i)
		}
		if len(u.Proxy) == 0 {
			return fmt.Errorf("upstream[%d]: at least one proxy entry is required", i)
		}
		for j, p := range u.Proxy {
			if p.ToInterface == "" {
				return fmt.Errorf("upstream[%d].proxy[%d]: to_interface is required", i, j)
			}
			if _, err := netip.ParseAddr(p.LANHostSuffix); err != nil {
				return fmt.Errorf("upstream[%d].proxy[%d].lan_host_suffix %q is not a valid IPv6 address: %w", i, j, p.LANHostSuffix, err)
			}
		}
	}
	return nil
}
