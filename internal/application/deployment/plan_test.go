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
	plan, err := Diff("cache", v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); got != "create:Network/net,create:Volume/cache,create:Volume/data,create:Config/conf,create:Workload/redis" {
		t.Fatalf("install plan: %s", got)
	}

	applied := appliedFrom(t, v1)
	plan, err = Diff("cache", manifest(6379), applied)
	if err != nil || plan.HasChanges() {
		t.Fatalf("same manifest: %s %v", actions(plan), err)
	}

	// Image change updates only the workload; a removed volume is deleted.
	v2 := manifest(6379)
	v2[0].Spec.(*resource.WorkloadSpec).Image = "redis:7.2"
	v2 = v2[:3] // drop cache volume and network...
	v2 = append(v2, manifest(6379)[4])
	plan, err = Diff("cache", v2, applied)
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
	plan, err := Diff("cache", v2, applied)
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
	_, err := Diff("cache", v2, applied)
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
	plan, err := Diff("db", []resource.Resource{secretRes("db", map[string]string{"password": "new-pass"})}, applied)
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
	plan, err := Diff("cache", v2, applied)
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

  WORKLOAD redis
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
	plan, err := Diff("cache", v2, applied)
	if err != nil || plan.HasChanges() {
		t.Fatalf("changing only the strategy must not change the workload: %s %v", actions(plan), err)
	}
}
