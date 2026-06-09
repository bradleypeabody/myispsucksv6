package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bradleypeabody/myispsucksv6/internal/config"
	"github.com/bradleypeabody/myispsucksv6/internal/hooks"
	"github.com/bradleypeabody/myispsucksv6/internal/lanaddr"
	"github.com/bradleypeabody/myispsucksv6/internal/ndp"
	"github.com/bradleypeabody/myispsucksv6/internal/netlinkmon"
	"github.com/bradleypeabody/myispsucksv6/internal/prefix"
	"github.com/bradleypeabody/myispsucksv6/internal/preflight"
	"github.com/bradleypeabody/myispsucksv6/internal/ra"
	"github.com/bradleypeabody/myispsucksv6/internal/state"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/myispsucksv6.jsonc", "path to config file")
		logFormat  = flag.String("log-format", "text", "log format: text or json")
	)
	flag.Parse()

	setupLogger(*logFormat, "info")

	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "dump":
			cmdDump(*configPath)
		case "test-hooks":
			cmdTestHooks(*configPath, args[1:])
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
			fmt.Fprintf(os.Stderr, "usage: myispsucksv6 [flags] [dump | test-hooks]\n")
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	setupLogger(*logFormat, cfg.Global.LogLevel)
	slog.Info("myispsucksv6 starting", "upstreams", len(cfg.Upstream))

	// Preflight: warn about misconfigured sysctls before doing anything else.
	for _, u := range cfg.Upstream {
		lans := make([]string, 0, len(u.Proxy))
		for _, p := range u.Proxy {
			lans = append(lans, p.ToInterface)
		}
		(&preflight.Checker{
			WANInterface:  u.Interface,
			LANInterfaces: lans,
		}).Check()
	}

	st, err := state.Load(cfg.Global.StateFile)
	if err != nil {
		slog.Warn("could not load state file (starting fresh)", "err", err)
		st = &state.State{Prefixes: make(map[string]string)}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	watcher := netlinkmon.NewWatcher()
	events, err := watcher.Subscribe(ctx)
	if err != nil {
		slog.Error("failed to subscribe to address events", "err", err)
		os.Exit(1)
	}

	// fanout routes the single subscription channel to per-upstream channels.
	fanout := make(map[string]chan netlinkmon.AddrEvent, len(cfg.Upstream))
	for _, u := range cfg.Upstream {
		fanout[u.Interface] = make(chan netlinkmon.AddrEvent, 8)
	}
	go func() {
		for ev := range events {
			if ch, ok := fanout[ev.Iface]; ok {
				select {
				case ch <- ev:
				default:
					slog.Warn("dropping addr event (channel full)", "iface", ev.Iface)
				}
			}
		}
	}()

	hookRunner := &hooks.Runner{Dir: cfg.Hooks.OnPrefixChangeDir}

	for _, u := range cfg.Upstream {
		filter, _ := netip.ParsePrefix(u.PrefixFilter)
		initial, _ := st.Get(u.Interface)

		// Build per-proxy managers.
		lanMgrs := make([]*lanaddr.Manager, len(u.Proxy))
		raEmitters := make([]*ra.Emitter, len(u.Proxy))
		ndpProxies := make([]*ndp.Proxy, len(u.Proxy))
		for i, p := range u.Proxy {
			suffix, _ := netip.ParseAddr(p.LANHostSuffix)
			lanMgrs[i] = &lanaddr.Manager{
				Interface: p.ToInterface,
				Suffix:    suffix,
			}
			raEmitters[i] = &ra.Emitter{
				Interface:         p.ToInterface,
				Suffix:            suffix,
				Disable:           p.DisableRA,
				IntervalSeconds:   p.RAIntervalSeconds,
				RouterLifetime:    p.RARouterLifetime,
				MTU:               p.RAMTU,
				DNSServers:        p.RADNSServers,
				ValidLifetime:     p.RAValidLifetime,
				PreferredLifetime: p.RAPreferredLifetime,
			}
			if !p.DisableNDPProxy {
				ndpProxies[i] = &ndp.Proxy{
					WANInterface: u.Interface,
					LANInterface: p.ToInterface,
				}
			}
			// Seed from state so managers have the current prefix before first event.
			if initial.IsValid() {
				lanMgrs[i].SetPrefix(initial)
				raEmitters[i].SetPrefix(initial)
				if ndpProxies[i] != nil {
					ndpProxies[i].SetPrefix(initial)
				}
			}
			em := raEmitters[i]
			go func() {
				if err := em.Run(ctx); err != nil {
					slog.Error("ra emitter failed", "iface", em.Interface, "err", err)
				}
			}()
			if pr := ndpProxies[i]; pr != nil {
				go func() {
					if err := pr.Run(ctx); err != nil {
						slog.Error("ndp proxy failed", "wan", pr.WANInterface, "lan", pr.LANInterface, "err", err)
					}
				}()
			}
		}

		mgr := &prefix.Manager{
			Interface:     u.Interface,
			Filter:        filter,
			Debounce:      time.Duration(u.DebounceSeconds) * time.Second,
			InitialPrefix: initial,
			OnChange: func(c prefix.Change) {
				for _, lm := range lanMgrs {
					lm.SetPrefix(c.New)
				}
				for _, em := range raEmitters {
					em.SetPrefix(c.New)
				}
				for _, pr := range ndpProxies {
					if pr != nil {
						pr.SetPrefix(c.New)
					}
				}

				newStr, oldStr := "", ""
				if c.New.IsValid() {
					newStr = c.New.String()
				}
				if c.Old.IsValid() {
					oldStr = c.Old.String()
				}
				hookRunner.Run(string(c.Action), c.Interface, newStr, oldStr)

				if c.New.IsValid() {
					st.Set(c.Interface, c.New)
				} else {
					st.Delete(c.Interface)
				}
				if err := st.Save(cfg.Global.StateFile); err != nil {
					slog.Warn("failed to save state", "err", err)
				}
			},
		}

		liveAddrs, err := watcher.CurrentAddrs(u.Interface)
		if err != nil {
			slog.Warn("could not query current addrs at startup", "iface", u.Interface, "err", err)
		} else {
			mgr.SeedFromLive(liveAddrs)
		}

		go mgr.Run(ctx, fanout[u.Interface])
	}

	<-ctx.Done()
	slog.Info("shutting down")
}

func setupLogger(format, level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func cmdDump(configPath string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	st, err := state.Load(cfg.Global.StateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load state file: %v\n", err)
		st = &state.State{Prefixes: make(map[string]string)}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	type dumpOutput struct {
		Config *config.Config `json:"config"`
		State  *state.State   `json:"state"`
	}
	_ = enc.Encode(dumpOutput{Config: cfg, State: st})
}

func cmdTestHooks(configPath string, args []string) {
	fs := flag.NewFlagSet("test-hooks", flag.ExitOnError)
	oldPrefix := fs.String("old-prefix", "", "previous prefix (empty for ActionAdded)")
	newPrefix := fs.String("new-prefix", "", "new prefix (empty for ActionRemoved)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	action := "changed"
	switch {
	case *oldPrefix == "":
		action = "added"
	case *newPrefix == "":
		action = "removed"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	iface := ""
	if len(cfg.Upstream) > 0 {
		iface = cfg.Upstream[0].Interface
	}

	r := &hooks.Runner{Dir: cfg.Hooks.OnPrefixChangeDir}
	failed := r.Run(action, iface, *newPrefix, *oldPrefix)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d hook(s) failed\n", failed)
		os.Exit(failed)
	}
	fmt.Println("all hooks passed")
}
