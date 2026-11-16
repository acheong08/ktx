// Package internaldns is a Caddy app that publishes DNS A records for the
// sites Caddy serves under one suffix (e.g. internal.melzi.dev), pointing
// them at this machine's address on one network interface (e.g. tailscale0).
//
// It only ever touches names that end in the configured suffix. It never
// manages wildcard names. It deletes records it created earlier once their
// site is removed from the config, but only if the record still points at
// this machine.
//
//	{
//		internal_dns {
//			zone      melzi.dev
//			suffix    internal.melzi.dev
//			provider  cloudflare {env.CLOUDFLARE_API_TOKEN}
//			interface tailscale0
//		}
//	}
package internaldns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/libdns/libdns"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
	httpcaddyfile.RegisterGlobalOption("internal_dns", parseGlobalOption)
}

// Provider is what the DNS provider module must support.
type Provider interface {
	libdns.RecordGetter
	libdns.RecordSetter
	libdns.RecordDeleter
}

// App keeps DNS records in sync with the configured sites.
type App struct {
	// Zone is the DNS zone at the provider, e.g. "melzi.dev".
	Zone string `json:"zone"`
	// Suffix limits which site names are published: the suffix itself and
	// names ending in "."+Suffix. Must be inside Zone.
	Suffix string `json:"suffix"`
	// Interface whose first IPv4 address is published, e.g. "tailscale0".
	Interface string `json:"interface,omitempty"`
	// IP is a fixed address to publish instead of reading Interface.
	IP string `json:"ip,omitempty"`
	// TTL of created records. Default 5m.
	TTL caddy.Duration `json:"ttl,omitempty"`
	// Interval between re-syncs, which repair drift and follow address
	// changes. Every config load also syncs. Default 10m.
	Interval caddy.Duration `json:"interval,omitempty"`
	// ProviderRaw is the DNS provider module, e.g. cloudflare.
	ProviderRaw json.RawMessage `json:"provider,omitempty" caddy:"namespace=dns.providers inline_key=name"`

	ctx    caddy.Context
	log    *zap.Logger
	syncer *syncer
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "internal_dns", New: func() caddy.Module { return new(App) }}
}

// Provision checks the config and loads the DNS provider.
func (a *App) Provision(ctx caddy.Context) error {
	a.ctx, a.log = ctx, ctx.Logger()
	a.Zone = normalize(a.Zone)
	a.Suffix = normalize(a.Suffix)
	switch {
	case a.Zone == "":
		return errors.New("internal_dns: zone is required")
	case a.Suffix == "":
		return errors.New("internal_dns: suffix is required")
	case a.Suffix != a.Zone && !strings.HasSuffix(a.Suffix, "."+a.Zone):
		return fmt.Errorf("internal_dns: suffix %q is not inside zone %q", a.Suffix, a.Zone)
	case (a.Interface == "") == (a.IP == ""):
		return errors.New("internal_dns: set exactly one of interface or ip")
	case a.ProviderRaw == nil:
		return errors.New("internal_dns: provider is required")
	}
	if a.IP != "" {
		ip, err := netip.ParseAddr(a.IP)
		if err != nil || !ip.Is4() {
			return fmt.Errorf("internal_dns: ip %q is not an IPv4 address", a.IP)
		}
	}
	if a.TTL == 0 {
		a.TTL = caddy.Duration(5 * time.Minute)
	}
	if a.Interval == 0 {
		a.Interval = caddy.Duration(10 * time.Minute)
	}

	mod, err := ctx.LoadModule(a, "ProviderRaw")
	if err != nil {
		return fmt.Errorf("internal_dns: loading provider: %w", err)
	}
	p, ok := mod.(Provider)
	if !ok {
		return fmt.Errorf("internal_dns: provider %T can't get, set and delete records", mod)
	}
	a.syncer = &syncer{
		zone: a.Zone, suffix: a.Suffix, ttl: time.Duration(a.TTL), provider: p, log: a.log,
		statePath: filepath.Join(caddy.AppDataDir(), "internal_dns", a.Zone+".json"),
	}
	return nil
}

// Start syncs in the background now and then every Interval, until this
// config is unloaded. A reload starts a new App, so new sites sync at once.
func (a *App) Start() error {
	go func() {
		t := time.NewTicker(time.Duration(a.Interval))
		defer t.Stop()
		for {
			a.syncOnce()
			select {
			case <-a.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Stop is a no-op; the sync loop ends with the config's context.
func (a *App) Stop() error { return nil }

func (a *App) syncOnce() {
	ip, err := a.address()
	if err != nil {
		a.log.Warn("not syncing DNS: no address to publish", zap.Error(err))
		return
	}
	hosts, err := a.hosts()
	if err != nil {
		a.log.Warn("not syncing DNS: reading sites", zap.Error(err))
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	if err := a.syncer.sync(ctx, hosts, ip); err != nil && ctx.Err() == nil {
		a.log.Error("DNS sync failed", zap.Error(err))
	}
}

func (a *App) address() (netip.Addr, error) {
	if a.IP != "" {
		return netip.MustParseAddr(a.IP), nil
	}
	iface, err := net.InterfaceByName(a.Interface)
	if err != nil {
		return netip.Addr{}, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() {
				return ip.Unmap(), nil
			}
		}
	}
	return netip.Addr{}, fmt.Errorf("interface %s has no IPv4 address", a.Interface)
}

// hosts lists the host matchers of every HTTP route, which is where
// Caddyfile site addresses end up.
func (a *App) hosts() ([]string, error) {
	app, err := a.ctx.App("http")
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, srv := range app.(*caddyhttp.App).Servers {
		for _, route := range srv.Routes {
			for _, set := range route.MatcherSets {
				for _, m := range set {
					if h, ok := m.(caddyhttp.MatchHost); ok {
						hosts = append(hosts, h...)
					}
				}
			}
		}
	}
	return hosts, nil
}

// syncer does the record bookkeeping; it knows nothing about Caddy.
type syncer struct {
	zone, suffix string
	ttl          time.Duration
	provider     Provider
	statePath    string // names this app created, so it can remove them later
	log          *zap.Logger
}

// One sync at a time, across reloads that briefly overlap.
var syncMu sync.Mutex

// sync makes every managed host an A record for ip and removes records it
// created for hosts that are gone.
func (s *syncer) sync(ctx context.Context, hosts []string, ip netip.Addr) error {
	syncMu.Lock()
	defer syncMu.Unlock()

	want := s.managedNames(hosts)
	prev, err := s.loadState()
	if err != nil {
		return err
	}

	zone := s.zone + "."
	records, err := s.provider.GetRecords(ctx, zone)
	if err != nil {
		return fmt.Errorf("listing records in %s: %w", s.zone, err)
	}
	addrs := map[string][]netip.Addr{} // A records by name
	other := map[string]string{}       // a non-A/AAAA type that blocks an A record
	var aRecords = map[string][]libdns.Record{}
	for _, r := range records {
		rr := r.RR()
		switch rr.Type {
		case "A":
			if a, err := netip.ParseAddr(rr.Data); err == nil {
				addrs[rr.Name] = append(addrs[rr.Name], a)
				aRecords[rr.Name] = append(aRecords[rr.Name], r)
			}
		case "CNAME":
			other[rr.Name] = rr.Type
		}
	}

	var errs []error
	managed := []string{}
	for _, name := range want {
		fqdn := s.fqdn(name)
		if t, blocked := other[name]; blocked {
			s.log.Warn("not publishing: name already has a record of another type", zap.String("name", fqdn), zap.String("type", t))
			continue
		}
		managed = append(managed, name)
		if len(addrs[name]) == 1 && addrs[name][0] == ip {
			continue
		}
		_, err := s.provider.SetRecords(ctx, zone, []libdns.Record{libdns.Address{Name: name, TTL: s.ttl, IP: ip}})
		if err != nil {
			errs = append(errs, fmt.Errorf("setting %s: %w", fqdn, err))
			continue
		}
		s.log.Info("published DNS record", zap.String("name", fqdn), zap.String("ip", ip.String()))
	}

	for _, name := range prev {
		if slices.Contains(managed, name) {
			continue
		}
		cur := addrs[name]
		if len(cur) != 1 || cur[0] != ip {
			// Gone already, or someone repointed it: not ours to delete.
			continue
		}
		if _, err := s.provider.DeleteRecords(ctx, zone, aRecords[name]); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", s.fqdn(name), err))
			managed = append(managed, name) // keep tracking; retry next time
			continue
		}
		s.log.Info("removed DNS record for a site that no longer exists", zap.String("name", s.fqdn(name)))
	}

	sort.Strings(managed)
	if err := s.saveState(managed); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// managedNames returns the zone-relative names of hosts under the suffix.
func (s *syncer) managedNames(hosts []string) []string {
	var names []string
	for _, h := range hosts {
		h = normalize(h)
		if strings.Contains(h, "*") || strings.Contains(h, "{") {
			continue // wildcards and placeholders are not single names
		}
		if h != s.suffix && !strings.HasSuffix(h, "."+s.suffix) {
			continue
		}
		name := "@"
		if h != s.zone {
			name = strings.TrimSuffix(h, "."+s.zone)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *syncer) fqdn(name string) string {
	if name == "@" {
		return s.zone
	}
	return name + "." + s.zone
}

func (s *syncer) loadState() ([]string, error) {
	b, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.statePath, err)
	}
	return names, nil
}

func (s *syncer) saveState(names []string) error {
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(names, "", "  ")
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

func normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// parseGlobalOption reads the internal_dns global option.
func parseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	app := new(App)
	d.Next() // option name
	if d.NextArg() {
		return nil, d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "zone", "suffix", "interface", "ip":
			key := d.Val()
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			val := d.Val()
			if d.NextArg() {
				return nil, d.ArgErr()
			}
			switch key {
			case "zone":
				app.Zone = val
			case "suffix":
				app.Suffix = val
			case "interface":
				app.Interface = val
			case "ip":
				app.IP = val
			}
		case "ttl", "interval":
			key := d.Val()
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return nil, d.Errf("%s: %v", key, err)
			}
			if key == "ttl" {
				app.TTL = caddy.Duration(dur)
			} else {
				app.Interval = caddy.Duration(dur)
			}
		case "provider":
			if !d.NextArg() {
				return nil, d.ArgErr()
			}
			name := d.Val()
			unm, err := caddyfile.UnmarshalModule(d, "dns.providers."+name)
			if err != nil {
				return nil, err
			}
			app.ProviderRaw = caddyconfig.JSONModuleObject(unm, "name", name, nil)
		default:
			return nil, d.Errf("internal_dns: unknown option %q", d.Val())
		}
	}
	return httpcaddyfile.App{Name: "internal_dns", Value: caddyconfig.JSON(app, nil)}, nil
}

// Interface guards.
var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
