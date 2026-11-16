//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga/internal/application/deployment"
	"github.com/hasankaradmir/karga/internal/application/packages"
	"github.com/hasankaradmir/karga/internal/application/releases"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/ports"
	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

// service returns a release service on real Docker and a fresh state directory. Everything the
// release created is removed when the test ends, volumes included.
func service(t *testing.T, name string) (*releases.Service, ports.Runtime) {
	t.Helper()
	rt := newRuntime(t)
	home := t.TempDir()
	store, err := sqlite.Open(context.Background(), filepath.Join(home, "karga.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deployment.Delete(context.Background(), rt, home, name, true); err != nil {
			t.Errorf("cleanup %s: %v", name, err)
		}
		_ = store.Close()
	})
	return releases.New(store, func(context.Context) (ports.Runtime, error) { return rt, nil }, home, nil, nil), rt
}

func opts() releases.CommonOptions {
	return releases.CommonOptions{Timeout: 2 * time.Minute, HistoryMax: releases.DefaultHistoryMax}
}

func workload(t *testing.T, rt ports.Runtime, name string) ports.WorkloadStatus {
	t.Helper()
	st, err := rt.GetWorkload(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func currentRelease(t *testing.T, s *releases.Service, name string) release.Release {
	t.Helper()
	rels, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rels {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("release %s not listed", name)
	return release.Release{}
}

func TestRedisReleaseLifecycle(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-rel")
	s, rt := service(t, name)
	port := runtimetest.FreePort(t)
	values := func(tag string) packages.Options {
		return packages.Options{Sets: []string{"network.port=" + strconv.Itoa(port), "image.tag=" + tag}}
	}

	// Install an older Redis, then upgrade forward (downgrading would hit RDB format changes).
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../../examples/redis", Values: values("7.2")}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: "../../examples/redis", Values: values("7.2")})
	if err != nil || !res.NoOp {
		t.Fatalf("second install should be a no-op: %+v %v", res, err)
	}
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../../examples/redis", Values: values("7.4")}); err != nil {
		t.Fatal(err)
	}
	if st := workload(t, rt, name+"-redis"); st.Health != ports.HealthHealthy || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("after upgrade: %+v", st)
	}

	// A missing image fails before the running container is touched.
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: opts(), Name: name, Package: "../../examples/redis", Values: values("0.0.0-missing")}); err == nil {
		t.Fatal("upgrade to a missing image should fail")
	}
	if st := workload(t, rt, name+"-redis"); st.State != ports.StateRunning || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("running workload was touched by a failed pull: %+v", st)
	}
	if r := currentRelease(t, s, name); r.Status != release.StatusFailed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed upgrade: %+v", r)
	}

	// Rollback without a revision returns to the last good one.
	res, err = s.Rollback(ctx, releases.RollbackRequest{CommonOptions: opts(), Name: name})
	if err != nil || res.Revision != 4 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	rep, err := s.Status(ctx, name)
	if err != nil || rep.Release.Status != release.StatusDeployed {
		t.Fatalf("status: %+v %v", rep, err)
	}

	if err := s.Uninstall(ctx, releases.UninstallRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if objs := managed(t, rt, name); len(objs) != 1 || objs[0].Kind != ports.ObjectVolume {
		t.Fatalf("after uninstall only the volume should remain: %+v", objs)
	}
}

// TestFailedUpgradeKeepsHistory is the M3 exit criterion: a failed upgrade leaves revision 1 deployed
// and current, the failing container stopped but present, and rollback brings back a healthy state.
func TestFailedUpgradeKeepsHistory(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-probe")
	s, rt := service(t, name)
	pkgPath := "testdata/probe"
	common := opts()
	common.Timeout = 30 * time.Second

	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: pkgPath}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: pkgPath,
		Values: packages.Options{Sets: []string{"healthy=false"}}})
	if err == nil {
		t.Fatal("unhealthy upgrade should fail")
	}
	hist, err := s.History(ctx, name)
	if err != nil || len(hist) != 2 || hist[0].Status != release.RevisionDeployed || hist[1].Status != release.RevisionFailed {
		t.Fatalf("history: %+v %v", hist, err)
	}
	if r := currentRelease(t, s, name); r.CurrentRevision != 1 {
		t.Fatalf("current revision: %+v", r)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateExited {
		t.Fatalf("failed container should be stopped and kept: %+v", st)
	}

	res, err := s.Rollback(ctx, releases.RollbackRequest{CommonOptions: common, Name: name, Revision: 1})
	if err != nil || res.Revision != 3 {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if st := workload(t, rt, name+"-probe"); st.State != ports.StateRunning || st.Health != ports.HealthHealthy {
		t.Fatalf("after rollback: %+v", st)
	}
}
