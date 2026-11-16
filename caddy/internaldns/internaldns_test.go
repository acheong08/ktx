package internaldns

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "github.com/caddy-dns/cloudflare"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/libdns/libdns"
	"go.uber.org/zap"
)

// fakeDNS is an in-memory zone with libdns semantics.
type fakeDNS struct {
	rrs   []libdns.RR
	sets  int
	dels  int
	zones []string
}

func (f *fakeDNS) GetRecords(_ context.Context, zone string) ([]libdns.Record, error) {
	f.zones = append(f.zones, zone)
	var out []libdns.Record
	for _, rr := range f.rrs {
		out = append(out, rr)
	}
	return out, nil
}

func (f *fakeDNS) SetRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	f.sets++
	for _, r := range recs {
		rr := r.RR()
		f.rrs = slices.DeleteFunc(f.rrs, func(x libdns.RR) bool { return x.Name == rr.Name && x.Type == rr.Type })
		f.rrs = append(f.rrs, rr)
	}
	return recs, nil
}

func (f *fakeDNS) DeleteRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	f.dels++
	for _, r := range recs {
		rr := r.RR()
		f.rrs = slices.DeleteFunc(f.rrs, func(x libdns.RR) bool { return x.Name == rr.Name && x.Type == rr.Type && x.Data == rr.Data })
	}
	return recs, nil
}

func (f *fakeDNS) lookup(name string) []string {
	var out []string
	for _, rr := range f.rrs {
		if rr.Name == name {
			out = append(out, rr.Type+" "+rr.Data)
		}
	}
	return out
}

var ip = netip.MustParseAddr("100.64.0.14")

func newSyncer(t *testing.T, f *fakeDNS) *syncer {
	return &syncer{
		zone: "melzi.dev", suffix: "internal.melzi.dev", ttl: 5 * time.Minute, provider: f,
		statePath: filepath.Join(t.TempDir(), "state.json"), log: zap.NewNop(),
	}
}

func TestManagedNames(t *testing.T) {
	s := newSyncer(t, &fakeDNS{})
	got := s.managedNames([]string{
		"brain.internal.melzi.dev", "BRAIN.internal.melzi.dev.", "*.internal.melzi.dev",
		"internal.melzi.dev", "a.b.internal.melzi.dev", "melzi.dev", "www.melzi.dev",
		"evilinternal.melzi.dev", "internal.melzi.dev.evil.com", "{http.request.host}",
	})
	want := []string{"a.b.internal", "brain.internal", "internal"}
	if !slices.Equal(got, want) {
		t.Errorf("managedNames = %v, want %v", got, want)
	}
}

func TestSync(t *testing.T) {
	f := &fakeDNS{rrs: []libdns.RR{
		{Name: "@", Type: "A", Data: "203.0.113.1"},            // apex: never touched
		{Name: "www", Type: "CNAME", Data: "melzi.dev."},       // unrelated
		{Name: "old.internal", Type: "A", Data: "100.64.0.99"}, // someone else's
		{Name: "cname.internal", Type: "CNAME", Data: "x.example."},
	}}
	s := newSyncer(t, f)
	ctx := context.Background()

	err := s.sync(ctx, []string{"brain.internal.melzi.dev", "grafana.internal.melzi.dev", "cname.internal.melzi.dev", "melzi.dev"}, ip)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lookup("brain.internal"); !slices.Equal(got, []string{"A 100.64.0.14"}) {
		t.Errorf("brain = %v", got)
	}
	if got := f.lookup("@"); !slices.Equal(got, []string{"A 203.0.113.1"}) {
		t.Errorf("apex changed: %v", got)
	}
	if got := f.lookup("cname.internal"); !slices.Equal(got, []string{"CNAME x.example."}) {
		t.Errorf("CNAME name changed: %v", got)
	}
	if f.zones[0] != "melzi.dev." {
		t.Errorf("zone passed as %q", f.zones[0])
	}

	// Nothing to do the second time.
	sets := f.sets
	if err := s.sync(ctx, []string{"brain.internal.melzi.dev", "grafana.internal.melzi.dev"}, ip); err != nil {
		t.Fatal(err)
	}
	if f.sets != sets {
		t.Errorf("unchanged sync made %d writes", f.sets-sets)
	}

	// grafana removed from config: its record goes. old.internal was never ours.
	if err := s.sync(ctx, []string{"brain.internal.melzi.dev"}, ip); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup("grafana.internal"); got != nil {
		t.Errorf("grafana should be deleted: %v", got)
	}
	if got := f.lookup("old.internal"); got == nil {
		t.Error("record not created by us was deleted")
	}

	// A record repointed by hand is left alone when its site disappears.
	f.rrs = append(slices.DeleteFunc(f.rrs, func(x libdns.RR) bool { return x.Name == "brain.internal" }),
		libdns.RR{Name: "brain.internal", Type: "A", Data: "100.64.0.50"})
	if err := s.sync(ctx, nil, ip); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup("brain.internal"); !slices.Equal(got, []string{"A 100.64.0.50"}) {
		t.Errorf("hand-edited record touched: %v", got)
	}

	// Address change: records follow.
	if err := s.sync(ctx, []string{"brain.internal.melzi.dev"}, netip.MustParseAddr("100.64.0.20")); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup("brain.internal"); !slices.Equal(got, []string{"A 100.64.0.20"}) {
		t.Errorf("after IP change: %v", got)
	}
}

func TestParseGlobalOption(t *testing.T) {
	d := caddyfile.NewTestDispenser(`internal_dns {
		zone melzi.dev
		suffix internal.melzi.dev
		interface tailscale0
		ttl 2m
		provider cloudflare {env.CLOUDFLARE_API_TOKEN}
	}`)
	v, err := parseGlobalOption(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(v.(httpcaddyfile.App).Value)
	want := `{"zone":"melzi.dev","suffix":"internal.melzi.dev","interface":"tailscale0","ttl":120000000000,"provider":{"api_token":"{env.CLOUDFLARE_API_TOKEN}","name":"cloudflare"}}`
	if got != want {
		t.Errorf("json =\n%s\nwant\n%s", got, want)
	}
	if _, err := parseGlobalOption(caddyfile.NewTestDispenser(`internal_dns { bogus x }`), nil); err == nil {
		t.Error("unknown option accepted")
	}
}
