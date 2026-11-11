// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
	"github.com/yuuki/lustremesh/internal/supervisor"
	"github.com/yuuki/lustremesh/internal/target"
	"github.com/yuuki/lustremesh/internal/worker"
)

const inst = "ffff8f0b3c5e2000"

// fakeVisitor answers visits from a script: the result of each step of each
// target, "ok" when not listed. A timeout blocks one more worker.
type fakeVisitor struct {
	devs      []target.Device
	results   map[string]probe.Result // "testfs-OST0001/write" → result
	abandoned int
	stalls    map[string]supervisor.Stall
	discErr   error

	visits []string // "testfs-OST0001:rpc" or "testfs-OST0001:data"
}

func (f *fakeVisitor) Discover(context.Context) ([]target.Device, []string, error) {
	return f.devs, []string{"osc/odd"}, f.discErr
}

func (f *fakeVisitor) Visit(_ context.Context, dev target.Device, data *worker.Data) supervisor.VisitReport {
	kind := "rpc"
	if data != nil {
		kind = "data"
	}
	f.visits = append(f.visits, dev.Label()+":"+kind)
	rep := supervisor.VisitReport{State: "FULL"}
	broken := false
	for _, step := range supervisor.Steps(dev, data) {
		if broken {
			rep.Steps = append(rep.Steps, probe.Skipped(step, "earlier step"))
			continue
		}
		res, ok := f.results[dev.Label()+"/"+string(step)]
		if !ok {
			res = probe.ResultOK
		}
		rep.Steps = append(rep.Steps, probe.StepResult{Step: step, Result: res})
		if res == probe.ResultTimeout {
			f.abandoned++
			broken = true
		} else if !res.Answered() {
			broken = true
		}
	}
	return rep
}

func (f *fakeVisitor) Abandoned() int                      { return f.abandoned }
func (f *fakeVisitor) Stalls() map[string]supervisor.Stall { return f.stalls }

func dev(t *testing.T, name, nid string) target.Device {
	t.Helper()
	d, err := target.ParseDeviceName(name)
	if err != nil {
		t.Fatal(err)
	}
	d.ServerNID = nid
	return d
}

// cluster is one MDT and four OSTs on two servers, listed out of order.
func cluster(t *testing.T) []target.Device {
	return []target.Device{
		dev(t, "testfs-OST0003-osc-"+inst, "10.0.0.3@tcp"),
		dev(t, "testfs-OST0000-osc-"+inst, "10.0.0.2@tcp"),
		dev(t, "testfs-MDT0000-mdc-"+inst, "10.0.0.1@tcp"),
		dev(t, "testfs-OST0001-osc-"+inst, "10.0.0.2@tcp"),
		dev(t, "testfs-OST0002-osc-"+inst, "10.0.0.3@tcp"),
	}
}

var opts = Options{ProbeRoot: "/probe", ClientID: "node-a001", VerifyPin: true, LfsPath: "lfs", MaxAbandoned: 16}

// table renders a report as "MDT0000[rpc=ok] OST0000[rpc=ok open=ok ...]".
func table(r Report) string {
	var out []string
	for _, t := range r.Targets {
		var steps []string
		for _, s := range t.Steps {
			steps = append(steps, string(s.Step)+"="+string(s.Result))
		}
		out = append(out, strings.TrimPrefix(t.Target, "testfs-")+"["+strings.Join(steps, " ")+"]")
	}
	return strings.Join(out, "\n")
}

const allData = "rpc=ok open=ok verify=ok write=ok read=ok close=ok"
const noData = "rpc=ok open=skipped verify=skipped write=skipped read=skipped close=skipped"
const nothing = "rpc=skipped open=skipped verify=skipped write=skipped read=skipped close=skipped"

func TestHealthyCluster(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t)}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "MDT0000[rpc=ok]\nOST0000[" + allData + "]\nOST0001[" + allData + "]\nOST0002[" + allData + "]\nOST0003[" + allData + "]"
	if got := table(rep); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if rep.Failed() || rep.FS != "testfs" || rep.Blocked != 0 || len(rep.Skipped) != 1 {
		t.Errorf("report %+v", rep)
	}
	if got := strings.Join(v.visits, " "); got != "testfs-MDT0000:rpc testfs-OST0000:data testfs-OST0001:data testfs-OST0002:data testfs-OST0003:data" {
		t.Errorf("visits: %s", got)
	}
}

func TestWithoutProbeRootOnlyRPC(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t)}
	o := opts
	o.ProbeRoot = ""
	rep, err := Run(context.Background(), v, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range rep.Targets {
		if len(tg.Steps) != 1 || tg.Steps[0].Step != probe.StepRPC {
			t.Errorf("%s: %+v", tg.Target, tg.Steps)
		}
	}
}

func TestRPCTimeoutHoldsTheServer(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{"testfs-OST0000/rpc": probe.ResultTimeout}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "MDT0000[rpc=ok]\n" +
		"OST0000[rpc=timeout open=skipped verify=skipped write=skipped read=skipped close=skipped]\n" +
		"OST0001[" + nothing + "]\n" + // same server: not even an rpc probe
		"OST0002[" + allData + "]\nOST0003[" + allData + "]"
	if got := table(rep); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, visit := range v.visits {
		if strings.HasPrefix(visit, "testfs-OST0001") {
			t.Errorf("visited %s", visit)
		}
	}
	if !rep.Failed() || rep.Blocked != 1 {
		t.Errorf("failed=%v blocked=%d", rep.Failed(), rep.Blocked)
	}
}

func TestWriteTimeoutHoldsDataOnTheServer(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{"testfs-OST0002/write": probe.ResultTimeout}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "MDT0000[rpc=ok]\nOST0000[" + allData + "]\nOST0001[" + allData + "]\n" +
		"OST0002[rpc=ok open=ok verify=ok write=timeout read=skipped close=skipped]\n" +
		"OST0003[" + noData + "]" // same server: the rpc probe only
	if got := table(rep); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := v.visits[len(v.visits)-1]; got != "testfs-OST0003:rpc" {
		t.Errorf("last visit: %s", got)
	}
}

func TestMetadataTroubleHoldsAllData(t *testing.T) {
	for name, results := range map[string]map[string]probe.Result{
		"MDT disconnected": {"testfs-MDT0000/rpc": probe.ResultDisconnected},
		"MDT timeout":      {"testfs-MDT0000/rpc": probe.ResultTimeout},
		"MDT stuck":        {"testfs-MDT0000/rpc": probe.ResultStuck},
	} {
		v := &fakeVisitor{devs: cluster(t), results: results}
		rep, err := Run(context.Background(), v, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, tg := range rep.Targets[1:] {
			if got := table(Report{Targets: []Target{tg}}); !strings.HasSuffix(got, "["+noData+"]") {
				t.Errorf("%s: %s", name, got)
			}
		}
		for _, visit := range v.visits {
			if strings.HasSuffix(visit, ":data") {
				t.Errorf("%s: %s", name, visit)
			}
		}
	}

	// An open that times out is trouble on the metadata path too: one
	// blocked worker, not one per OST.
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{"testfs-OST0000/open": probe.ResultTimeout}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "MDT0000[rpc=ok]\n" +
		"OST0000[rpc=ok open=timeout verify=skipped write=skipped read=skipped close=skipped]\n" +
		"OST0001[" + noData + "]\nOST0002[" + noData + "]\nOST0003[" + noData + "]"
	if got := table(rep); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if rep.Blocked != 1 {
		t.Errorf("blocked = %d", rep.Blocked)
	}
}

func TestFailuresThatAreNotTimeoutsHoldNothing(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{
		"testfs-OST0000/rpc":   probe.ResultDisconnected,
		"testfs-OST0002/open":  probe.ResultUnprepared,
		"testfs-OST0003/write": probe.ResultError,
	}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(v.visits, " "); got != "testfs-MDT0000:rpc testfs-OST0000:data testfs-OST0001:data testfs-OST0002:data testfs-OST0003:data" {
		t.Errorf("visits: %s", got)
	}
	if !rep.Failed() {
		t.Error("not failed")
	}

	// An MDT that is deactivated, or whose probe failed for a reason of
	// ours, does not say that the metadata path would block.
	for _, res := range []probe.Result{probe.ResultInactive, probe.ResultError, probe.ResultSkipped} {
		v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{"testfs-MDT0000/rpc": res}}
		if _, err := Run(context.Background(), v, opts); err != nil {
			t.Fatal(err)
		}
		for _, visit := range v.visits[1:] {
			if !strings.HasSuffix(visit, ":data") {
				t.Errorf("MDT %s: %s", res, visit)
			}
		}
	}
}

func TestVerifyTimeoutHoldsDataOnTheServerOnly(t *testing.T) {
	// The verification can wait for the OST, so its timeout accuses the
	// server of that OST, not the metadata path.
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{"testfs-OST0000/verify": probe.ResultTimeout}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "MDT0000[rpc=ok]\n" +
		"OST0000[rpc=ok open=ok verify=timeout write=skipped read=skipped close=skipped]\n" +
		"OST0001[" + noData + "]\nOST0002[" + allData + "]\nOST0003[" + allData + "]"
	if got := table(rep); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestProbeLeftBlockedByAnEarlierRunHoldsItsServer(t *testing.T) {
	// The earlier run timed out on OST0000 and held OST0001. This run finds
	// OST0000 stuck: it must hold OST0001 again, not block a second worker.
	v := &fakeVisitor{devs: cluster(t), results: map[string]probe.Result{
		"testfs-OST0000/rpc": probe.ResultStuck,
		"testfs-OST0001/rpc": probe.ResultTimeout, // what would happen if it were probed
	}}
	o := opts
	o.StuckTargets = func() []string { return []string{"testfs-OST0000"} }
	rep, err := Run(context.Background(), v, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, visit := range v.visits {
		if strings.HasPrefix(visit, "testfs-OST0001") {
			t.Errorf("visited %s", visit)
		}
	}
	if v.abandoned != 0 {
		t.Errorf("%d more workers blocked", v.abandoned)
	}
	if got := table(Report{Targets: rep.Targets[2:3]}); got != "OST0001["+nothing+"]" {
		t.Errorf("OST0001: %s", got)
	}
	if len(rep.StuckTargets) != 1 || rep.StuckTargets[0] != "testfs-OST0000" || !rep.Failed() {
		t.Errorf("report: stuck %v", rep.StuckTargets)
	}
}

func TestBlockedProbesOfEarlierRunsCountTowardsTheLimit(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t)}
	o := opts
	o.MaxAbandoned = 2
	o.StuckTargets = func() []string { return []string{"other-OST0000", "other-OST0001"} }
	rep, err := Run(context.Background(), v, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.visits) != 0 {
		t.Errorf("visits %v", v.visits)
	}
	if first := rep.Targets[0].Steps[0]; first.Result != probe.ResultSkipped || !strings.Contains(first.Detail, "--max-abandoned") {
		t.Errorf("first target: %+v", first)
	}
}

func TestUnknownServersAreNotGrouped(t *testing.T) {
	devs := []target.Device{
		dev(t, "testfs-OST0000-osc-"+inst, target.UnknownNID),
		dev(t, "testfs-OST0001-osc-"+inst, target.UnknownNID),
	}
	v := &fakeVisitor{devs: devs, results: map[string]probe.Result{"testfs-OST0000/rpc": probe.ResultTimeout}}
	if _, err := Run(context.Background(), v, opts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(v.visits, " "); got != "testfs-OST0000:data testfs-OST0001:data" {
		t.Errorf("visits: %s", got)
	}
}

func TestMaxAbandonedStopsTheProbes(t *testing.T) {
	// Every OST is on its own server and blocks: no hold applies, the limit does.
	var devs []target.Device
	results := map[string]probe.Result{}
	for i, name := range []string{"0000", "0001", "0002", "0003", "0004"} {
		d := dev(t, "testfs-OST"+name+"-osc-"+inst, "10.0.1."+string(rune('0'+i))+"@tcp")
		devs = append(devs, d)
		results[d.Label()+"/rpc"] = probe.ResultTimeout
	}
	v := &fakeVisitor{devs: devs, results: results}
	o := opts
	o.MaxAbandoned = 2
	rep, err := Run(context.Background(), v, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.visits) != 2 || rep.Blocked != 2 {
		t.Errorf("visits %v, blocked %d", v.visits, rep.Blocked)
	}
	last := rep.Targets[4].Steps[0]
	if last.Result != probe.ResultSkipped || !strings.Contains(last.Detail, "--max-abandoned") {
		t.Errorf("last target: %+v", last)
	}
}

func TestInterruptedRunSkipsTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := &fakeVisitor{devs: cluster(t)}
	rep, err := Run(ctx, v, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.visits) != 0 || !rep.Failed() {
		t.Errorf("visits %v", v.visits)
	}
}

func TestRunErrors(t *testing.T) {
	if _, err := Run(context.Background(), &fakeVisitor{discErr: errors.New("no sysfs")}, opts); err == nil {
		t.Error("discover error not returned")
	}
	if _, err := Run(context.Background(), &fakeVisitor{}, opts); !errors.Is(err, target.ErrNoDevices) {
		t.Errorf("no devices: %v", err)
	}
	two := []target.Device{dev(t, "alpha-OST0000-osc-"+inst, "a"), dev(t, "beta-OST0000-osc-"+inst, "b")}
	if _, err := Run(context.Background(), &fakeVisitor{devs: two}, opts); err == nil {
		t.Error("two file systems: no error")
	}
}

func TestStallIsAttachedToItsTarget(t *testing.T) {
	v := &fakeVisitor{devs: cluster(t), stalls: map[string]supervisor.Stall{"testfs-OST0001": {Step: probe.StepWrite, Nanos: 37e9}}}
	rep, err := Run(context.Background(), v, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range rep.Targets {
		if (tg.Stall != nil) != (tg.Target == "testfs-OST0001") {
			t.Errorf("%s: stall %+v", tg.Target, tg.Stall)
		}
	}
}

func ns(v int64) *int64 { return &v }

func sampleReport() Report {
	return Report{
		FS: "testfs", ClientID: "node-a001", ProbeRoot: "/mnt/testfs/.lustremesh",
		Targets: []Target{
			{Target: "testfs-MDT0000", Kind: target.KindMDT, ServerNID: "10.0.0.1@tcp", State: "FULL",
				Steps: []probe.StepResult{{Step: probe.StepRPC, Result: probe.ResultOK, Nanos: ns(312_400), RPCSent: probe.RPCSentUnknown}}},
			{Target: "testfs-OST0000", Kind: target.KindOST, ServerNID: "10.0.0.2@tcp", State: "IDLE",
				Steps: []probe.StepResult{
					{Step: probe.StepRPC, Result: probe.ResultOK, Nanos: ns(2_104_000), RPCSent: probe.RPCSentYes},
					{Step: probe.StepOpen, Result: probe.ResultOK, Nanos: ns(410_000)},
					{Step: probe.StepVerify, Result: probe.ResultOK},
					{Step: probe.StepWrite, Result: probe.ResultOK, Nanos: ns(1_912_000)},
					{Step: probe.StepRead, Result: probe.ResultContended, Nanos: ns(640_000), Detail: "waited 200µs for a CPU"},
					{Step: probe.StepClose, Result: probe.ResultOK},
				}},
			{Target: "testfs-OST0001", Kind: target.KindOST, Index: 1, ServerNID: "10.0.0.2@tcp", State: "FULL",
				Steps: []probe.StepResult{
					{Step: probe.StepRPC, Result: probe.ResultOK, Nanos: ns(300_000), RPCSent: probe.RPCSentYes},
					{Step: probe.StepOpen, Result: probe.ResultOK, Nanos: ns(400_000)},
					{Step: probe.StepVerify, Result: probe.ResultOK},
					{Step: probe.StepWrite, Result: probe.ResultTimeout, Detail: "no answer within 5s"},
					probe.Skipped(probe.StepRead, "write timed out"),
					probe.Skipped(probe.StepClose, "write timed out"),
				},
				Stall: &supervisor.Stall{Step: probe.StepWrite, Nanos: 37_210_000_000}},
			{Target: "testfs-OST0002", Kind: target.KindOST, Index: 2, ServerNID: "unknown", State: "unknown",
				Steps: []probe.StepResult{
					{Step: probe.StepRPC, Result: probe.ResultDisconnected, Errno: "EAGAIN", Nanos: ns(9_000)},
					probe.Skipped(probe.StepOpen, "the rpc probe was disconnected"),
					probe.Skipped(probe.StepWrite, "the rpc probe was disconnected"),
				}},
		},
		Skipped:      []string{"osc/testfs-OST0000-osc-MDT0000"},
		Blocked:      1,
		StuckTargets: []string{"testfs-OST0001", "testfs-OST0007"},
	}
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteText(&buf, sampleReport()); err != nil {
		t.Fatal(err)
	}
	const want = `TARGET          SERVER        STATE    RPC                      OPEN      WRITE      READ             NOTE
testfs-MDT0000  10.0.0.1@tcp  FULL     ok 312µs                 -         -          -
testfs-OST0000  10.0.0.2@tcp  IDLE     ok 2.1ms                 ok 410µs  ok 1.91ms  contended 640µs  read: waited 200µs for a CPU
testfs-OST0001  10.0.0.2@tcp  FULL     ok 300µs                 ok 400µs  timeout    skipped          write: no answer within 5s; skipped: write timed out; write returned after 37.2s
testfs-OST0002  unknown       unknown  disconnected EAGAIN 9µs  skipped   skipped    -                skipped: the rpc probe was disconnected

4 targets of testfs, 2 with a step that got no answer; 1 worker(s) still blocked in a probe and left behind
2 target(s) with a probe still blocked, from this run or an earlier one: testfs-OST0001, testfs-OST0007
1 rpc probe(s) ok without proof that an RPC was sent: the RPC counters (debugfs) are not readable
not client imports, ignored: osc/testfs-OST0000-osc-MDT0000
`
	if got := buf.String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	in := sampleReport()
	if err := WriteJSON(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out Report
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.FS != in.FS || len(out.Targets) != len(in.Targets) || out.Blocked != 1 {
		t.Errorf("round trip: %+v", out)
	}
	// Field names are part of the report's contract.
	for _, field := range []string{`"fs"`, `"client_id"`, `"targets"`, `"target"`, `"server_nid"`, `"state"`, `"steps"`, `"step"`, `"result"`, `"nanos"`, `"errno"`, `"detail"`, `"rpc_sent"`, `"stall"`, `"blocked_workers"`, `"stuck_targets"`, `"skipped_devices"`} {
		if !strings.Contains(buf.String(), field) {
			t.Errorf("no %s in the JSON report", field)
		}
	}
	// An unknown duration is absent, not zero.
	if strings.Contains(buf.String(), `"nanos": 0`) {
		t.Error("a zero duration was written")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[int64]string{
		0: "0s", 940: "940ns", 1_394: "1.39µs", 105_420: "105µs", 2_104_000: "2.1ms",
		999_999_999: "1s", 37_210_000_000: "37.2s", 3_600_000_000_000: "1h0m0s",
	}
	for in, want := range tests {
		if got := formatDuration(durationOf(in)); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

func durationOf(nanos int64) time.Duration { return time.Duration(nanos) }

func TestTextReportKeepsCommandOutputOnOneLine(t *testing.T) {
	rep := Report{FS: "testfs", Targets: []Target{{
		Target: "testfs-OST0000", ServerNID: "10.0.0.2@tcp", State: "FULL",
		Steps: []probe.StepResult{
			{Step: probe.StepRPC, Result: probe.ResultOK},
			{Step: probe.StepVerify, Result: probe.ResultUnprepared, Detail: "lfs: two\nlines\tand \x1b[31mcolour"},
		},
	}}}
	var buf bytes.Buffer
	if err := WriteText(&buf, rep); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 || strings.ContainsAny(buf.String(), "\t\x1b") || !strings.Contains(lines[1], "verify: lfs: two lines and [31mcolour") {
		t.Errorf("got %q", buf.String())
	}
}
