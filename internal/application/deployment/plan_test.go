package deployment

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/domain/resource"
)

func appliedFrom(t *testing.T, rs []resource.Resource) []release.AppliedResource {
	t.Helper()
	specs, err := Specs(rs)
	if err != nil {
		t.Fatal(err)
	}
	var out []release.AppliedResource
	for _, s := range specs {
		out = append(out, s)
	}
	return out
}

func actions(p Plan) string {
	var s []string
	for _, c := range p.Changes {
		s = append(s, c.Action+":"+c.Kind+"/"+c.Name)
	}
	return strings.Join(s, ",")
}

func TestDiffCreateUnchangedUpdateDelete(t *testing.T) {
	v1 := manifest(6379)
	plan, err := Diff("cache", v1, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); got != "create:Network/net,create:Volume/cache,create:Volume/data,create:Config/conf,create:Workload/redis" {
		t.Fatalf("install plan: %s", got)
	}

	applied := appliedFrom(t, v1)
	plan, err = Diff("cache", manifest(6379), applied, "")
	if err != nil || plan.HasChanges() {
		t.Fatalf("same manifest: %s %v", actions(plan), err)
	}

	// Image change updates only the workload; a removed volume is deleted.
	v2 := manifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	v2 = v2[:3] // drop cache volume and network...
	v2 = append(v2, manifest(6379)[4])
	plan, err = Diff("cache", v2, applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); got != "unchanged:Network/net,unchanged:Volume/data,unchanged:Config/conf,update:Workload/redis,delete:Volume/cache" {
		t.Fatalf("upgrade plan: %s", got)
	}
	var w Change
	for _, c := range plan.Changes {
		if c.Kind == resource.KindWorkload {
			w = c
		}
	}
	if len(w.Fields) != 1 || w.Fields[0] != (FieldChange{Path: "spec.image", Old: `"redis:7.4"`, New: `"redis:7.2"`}) {
		t.Fatalf("fields: %+v", w.Fields)
	}
}

func TestDiffConfigChangeRecreatesWorkload(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	v2 := manifest(6379)
	v2[1] = cfgRes("conf", map[string]string{"redis.conf": "changed"})
	plan, err := Diff("cache", v2, applied, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !strings.Contains(got, "update:Config/conf") || !strings.Contains(got, "update:Workload/redis") {
		t.Fatalf("plan: %s", got)
	}
}

func TestDiffRejectsNetworkAndVolumeChanges(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	v2 := manifest(6379)
	v2[4].Spec = &resource.NetworkSpec{Driver: "bridge", Internal: true}
	v2[2].Spec = &resource.VolumeSpec{Driver: "local", Retention: "delete"}
	_, err := Diff("cache", v2, applied, "")
	if err == nil || !strings.Contains(err.Error(), `changing Network "net" is not supported`) || !strings.Contains(err.Error(), `changing Volume "data"`) {
		t.Fatalf("want rejection, got %v", err)
	}
}

func TestSecretsRedactedInPlan(t *testing.T) {
	v1 := []resource.Resource{secretRes("db", map[string]string{"password": "old-pass"})}
	applied := appliedFrom(t, v1)
	if strings.Contains(string(applied[0].Spec), "old-pass") {
		t.Fatalf("secret stored in plain text: %s", applied[0].Spec)
	}
	plan, err := Diff("db", []resource.Resource{secretRes("db", map[string]string{"password": "new-pass"})}, applied, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := plan.Print(&buf, "db", 1, 2); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "pass\"") || strings.Contains(out, "sha256") || !strings.Contains(out, "spec.values.password: (changed)") {
		t.Fatalf("plan output:\n%s", out)
	}
}

func TestPlanPrint(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	v2 := manifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	plan, err := Diff("cache", v2, applied, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := plan.Print(&buf, "cache", 2, 3); err != nil {
		t.Fatal(err)
	}
	want := `Deployment Plan

Release: cache
Revision: 2 -> 3

Changes:

  NETWORK net
    unchanged

  VOLUME cache
    unchanged

  VOLUME data
    unchanged

  CONFIG conf
    unchanged

  WORKLOAD redis (recreate)
    spec.image: "redis:7.4" -> "redis:7.2"
`
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestStrategyNotInDigest(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	v2 := manifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace}
	plan, err := Diff("cache", v2, applied, "")
	if err != nil || plan.HasChanges() {
		t.Fatalf("changing only the strategy must not change the workload: %s %v", actions(plan), err)
	}
}

func TestStrategyResolution(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	plan := func(mutate func(*resource.WorkloadSpec), override string) Change {
		t.Helper()
		v2 := manifest(6379)
		w := v2[0].Spec.(*resource.WorkloadSpec)
		w.Image = "redis:7.2"
		mutate(w)
		p, err := Diff("cache", v2, applied, override)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Changes {
			if c.Kind == resource.KindWorkload {
				return c
			}
		}
		t.Fatal("no workload change")
		return Change{}
	}
	eligible := func(w *resource.WorkloadSpec) {
		w.UpdateStrategy = &resource.UpdateStrategy{Type: resource.StrategyReplace}
		w.Mounts[0].ReadOnly = true
	}
	tests := []struct {
		name     string
		mutate   func(*resource.WorkloadSpec)
		override string
		want     string
	}{
		{"default recreate", func(*resource.WorkloadSpec) {}, "", "recreate"},
		{"eligible replace", eligible, "", "replace"},
		{"override to recreate", eligible, "recreate", "recreate"},
		{"override to replace", func(w *resource.WorkloadSpec) { eligible(w); w.UpdateStrategy = nil }, "replace", "replace"},
		{"rw volume", func(w *resource.WorkloadSpec) { eligible(w); w.Mounts[0].ReadOnly = false }, "", "recreate: mounts volume data read-write"},
		{"no healthcheck", func(w *resource.WorkloadSpec) { eligible(w); w.HealthCheck = nil }, "", "recreate: no healthCheck to verify readiness"},
		{"anonymous volume", func(w *resource.WorkloadSpec) { eligible(w); w.AnonymousVolumes = []string{"/cache"} }, "replace", "recreate: keeps image VOLUME /cache in an anonymous volume"},
	}
	for _, tt := range tests {
		if got := plan(tt.mutate, tt.override).Label(); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
	if _, err := Diff("cache", manifest(6379), applied, "rolling"); err == nil {
		t.Error("unknown strategy: expected error")
	}
}

func TestTLSSecretChangeUpdatesService(t *testing.T) {
	withProxySupport(t, true)
	tlsManifest := func(cert string) []resource.Resource {
		rs := append(manifest(6379), serviceRes("redis", "redis", 443, "redis",
			resource.Exposure{Port: "p", Listener: "websecure", Host: "a.example", Path: "/", TLS: &resource.ExposureTLS{Secret: "cert"}}))
		return append(rs, secretRes("cert", map[string]string{resource.TLSCertKey: cert, resource.TLSKeyKey: "k"}))
	}
	ordered, err := Order(tlsManifest("c1"))
	if err != nil {
		t.Fatal(err)
	}
	if n := names(ordered); strings.Index(n, "Secret/cert") > strings.Index(n, "Service/redis") || strings.Index(n, "Workload/redis") > strings.Index(n, "Service/redis") {
		t.Fatalf("the tls secret and the workload come before the service: %s", n)
	}
	p, err := Diff("cache", tlsManifest("c2"), appliedFrom(t, tlsManifest("c1")), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(p); !strings.Contains(got, "update:Service/redis") || !strings.Contains(got, "update:Secret/cert") || strings.Contains(got, "update:Workload/redis") {
		t.Fatalf("a new certificate updates the service only: %s", got)
	}
}

func TestServicesNeedTheGateway(t *testing.T) {
	withProxySupport(t, false)
	_, err := Diff("cache", append(manifest(6379), serviceRes("redis", "redis", 6379, "redis")), nil, "")
	if err == nil || !strings.Contains(err.Error(), "services redis need karga's gateway") {
		t.Fatalf("got %v", err)
	}
}

func TestDigestSchemeUpgrade(t *testing.T) {
	rs := append(manifest(0), secretRes("creds", map[string]string{"password": "pw"}))
	rs[0].Spec.(*resource.WorkloadSpec).Mounts = append(rs[0].Spec.(*resource.WorkloadSpec).Mounts, resource.Mount{Secret: "creds", MountPath: "/run/creds"})
	old := appliedFrom(t, rs) // written by a karga without keyed digests
	SetDigestKey([]byte("0123456789abcdef0123456789abcdef"))
	t.Cleanup(func() { SetDigestKey(nil) })
	p, err := Diff("cache", rs, old, "")
	if err != nil {
		t.Fatal(err)
	}
	sec := changeOf(t, p, resource.KindSecret, "creds")
	if sec.Action != ActionUpdate || len(sec.Fields) != 1 || sec.Fields[0].New != "(digest scheme upgraded)" {
		t.Fatalf("secret: %+v", sec)
	}
	if w := changeOf(t, p, resource.KindWorkload, "redis"); w.Action != ActionUpdate {
		t.Fatalf("the workload mounting it is applied once more: %+v", w)
	}
	again, err := Diff("cache", rs, appliedFrom(t, rs), "")
	if err != nil || again.HasChanges() {
		t.Fatalf("second plan must be unchanged: %s %v", actions(again), err)
	}
}

func TestPlanSummary(t *testing.T) {
	applied := appliedFrom(t, manifest(6379))
	v2 := manifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	v2 = append(v2[:3], v2[4:]...) // drop the "cache" volume
	p, err := Diff("cache", v2, applied, "")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := p.Summary(&b, "cache", 1, 2); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"~ WORKLOAD redis (recreate)", "- VOLUME cache", "0 to create, 1 to update, 1 to delete, 3 unchanged."} {
		if !strings.Contains(out, want) {
			t.Errorf("summary misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NETWORK net") || strings.Contains(out, "spec.image") {
		t.Errorf("summary shows unchanged resources or fields:\n%s", out)
	}
	b.Reset()
	if err := (Plan{}).Summary(&b, "cache", 2, 3); err != nil || !strings.Contains(b.String(), "No changes.") {
		t.Fatalf("empty plan: %q %v", b.String(), err)
	}
}
