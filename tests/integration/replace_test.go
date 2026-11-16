//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga/internal/application/packages"
	"github.com/hasankaradmir/karga/internal/application/releases"
	"github.com/hasankaradmir/karga/internal/domain/release"
	"github.com/hasankaradmir/karga/internal/ports"
)

const webPkg = "testdata/web"

// startDelay makes the server take 3s to listen: a recreate then has a real outage, a replace must
// hide it.
const startDelay = "startDelay=3"

// maxRaceFailures bounds requests that may fail while racing a replace switch-over (see below).
const maxRaceFailures = 2

// startPinger runs a client on the release network that requests http://web:8080/ every 100ms and
// prints the body, or FAIL when a request fails.
func startPinger(t *testing.T, rt ports.Runtime, rel string) string {
	t.Helper()
	ctx := context.Background()
	name := rel + "-pinger"
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{
		Name: name, Image: "busybox:1.37", RestartPolicy: "never",
		Labels:   map[string]string{ports.LabelManaged: "true", ports.LabelRelease: rel},
		Networks: []string{rel + "-web-net"},
		Command:  []string{"sh", "-c", `while :; do wget -q -T 2 -O - http://web:8080/ || echo FAIL; echo; sleep 0.1; done`},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, name); err != nil {
		t.Fatal(err)
	}
	return name
}

func logs(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "logs", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs %s: %v\n%s", container, err, out)
	}
	return string(out)
}

func TestReplaceWithoutDowntime(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-web")
	s, rt := service(t, name)
	common := opts()
	common.Timeout = 30 * time.Second
	upgrade := func(sets ...string) (*releases.Result, error) {
		return s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg,
			Values: packages.Options{Sets: append([]string{startDelay}, sets...)}})
	}

	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{startDelay}}}); err != nil {
		t.Fatal(err)
	}
	pinger := startPinger(t, rt, name)
	time.Sleep(time.Second)

	res, err := upgrade("content=v2")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Plan.Changes {
		if c.Kind == "Workload" && c.Strategy != "replace" {
			t.Fatalf("expected replace, got %+v", c)
		}
	}
	time.Sleep(time.Second)

	// A broken new version never takes traffic and the old one keeps serving.
	if _, err := upgrade("content=v3", "healthy=false"); err == nil {
		t.Fatal("unhealthy replace should fail")
	}
	time.Sleep(time.Second)

	out := logs(t, pinger)
	// There is no outage window, but a request that resolved the old container's IP just before it
	// stopped can still fail: DNS-based switching cannot drain that race (a retrying proxy, phase 2,
	// can). Allow at most maxRaceFailures; the recreate control shows a real outage is far larger.
	t.Logf("replace: %d of %d requests failed", strings.Count(out, "FAIL"), strings.Count(out, "\n")/2)
	if n := strings.Count(out, "FAIL"); n > maxRaceFailures {
		t.Fatalf("%d failed requests during replace (max %d):\n%s", n, maxRaceFailures, out)
	}
	if !strings.Contains(out, "v1") || !strings.Contains(out, "v2") || strings.Contains(out, "v3") {
		t.Fatalf("unexpected content sequence:\n%s", out)
	}

	st := workload(t, rt, name+"-web")
	if st.State != ports.StateRunning || st.Labels[ports.LabelRevision] != "2" {
		t.Fatalf("canonical container: %+v", st)
	}
	r := currentRelease(t, s, name)
	if r.Status != release.StatusDeployed || r.CurrentRevision != 2 {
		t.Fatalf("release after failed replace: %+v", r)
	}
	hist, err := s.History(ctx, name)
	if err != nil || len(hist) != 3 || hist[2].Status != release.RevisionFailed {
		t.Fatalf("history: %+v %v", hist, err)
	}
}

// TestRecreateHasDowntime is the control for TestReplaceWithoutDowntime: the same upgrade with
// recreate must be seen by the pinger, otherwise the pinger proves nothing.
func TestRecreateHasDowntime(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-web-rc")
	s, rt := service(t, name)
	common := opts()
	common.Timeout = 30 * time.Second
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{startDelay}}}); err != nil {
		t.Fatal(err)
	}
	pinger := startPinger(t, rt, name)
	time.Sleep(time.Second)
	if _, err := s.Upgrade(ctx, releases.UpgradeRequest{CommonOptions: common, Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{startDelay, "content=v2"}}, Strategy: "recreate"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	out := logs(t, pinger)
	t.Logf("recreate: %d of %d requests failed", strings.Count(out, "FAIL"), strings.Count(out, "\n")/2)
	if n := strings.Count(out, "FAIL"); n <= 3*maxRaceFailures {
		t.Fatalf("recreate showed only %d failures; the pinger cannot tell an outage from the replace race:\n%s", n, out)
	}
}
