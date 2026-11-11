// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseStatCount(t *testing.T) {
	// Hand-written in the public format of a Lustre stats file.
	const stats = `snapshot_time             1700000000.000000000 secs.nsecs
req_waittime              120 samples [usec] 50 9000 60000 400000
ost_statfs                17 samples [usec] 80 400 2000 90000
ost_statfs_extra          99 samples [usec] 1 1 1 1
`
	if n, ok := ParseStatCount([]byte(stats), "ost_statfs"); !ok || n != 17 {
		t.Errorf("ost_statfs = %d, %v", n, ok)
	}
	if _, ok := ParseStatCount([]byte(stats), "mds_statfs"); ok {
		t.Error("mds_statfs found in a file without it")
	}
	if _, ok := ParseStatCount([]byte("ost_statfs many samples\n"), "ost_statfs"); ok {
		t.Error("a count that is not a number was accepted")
	}
	if _, ok := ParseStatCount(nil, "ost_statfs"); ok {
		t.Error("empty file")
	}
}

func TestParseImportState(t *testing.T) {
	tests := map[string]string{
		"current_state: FULL\nstate_history:\n - [ 1700000000, CONNECTING ]\n": "FULL",
		"current_state: IDLE\n":           "IDLE",
		"  current_state:   REPLAY_WAIT ": "REPLAY_WAIT",
		"":                                StateUnknown,
		"state: FULL\n":                   StateUnknown,
		"current_state: full\n":           StateUnknown,
		"current_state: \n":               StateUnknown,
		"current_state: " + strings.Repeat("A", 40) + "\n": StateUnknown,
	}
	for in, want := range tests {
		if got := ParseImportState([]byte(in)); got != want {
			t.Errorf("ParseImportState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRunDelay(t *testing.T) {
	if d, ok := ParseRunDelay([]byte("123456 7890 12\n")); !ok || d != 7890*time.Nanosecond {
		t.Errorf("got %v, %v", d, ok)
	}
	for _, in := range []string{"", "0 0 0\n", "1 2\n", "1 2 3 4\n", "a b c\n", "1 -2 3\n", "1 99999999999999999999 3\n", "1 9223372036854775808 3\n"} {
		if d, ok := ParseRunDelay([]byte(in)); ok {
			t.Errorf("ParseRunDelay(%q) = %v, want not ok", in, d)
		}
	}
}

// plainLayout is hand-written in the format "lfs getstripe <file>" prints
// for a file with one stripe.
const plainLayout = `/mnt/testfs/.lustremesh/v1/ost/0003/node-a001
lmm_stripe_count:  1
lmm_stripe_size:   1048576
lmm_pattern:       raid0
lmm_layout_gen:    0
lmm_stripe_offset: 3
	obdidx		 objid		 objid		 group
	     3	           514	        0x202	             0

`

func TestParsePlainLayout(t *testing.T) {
	if n, err := ParsePlainLayout([]byte(plainLayout)); err != nil || n != 3 {
		t.Errorf("plain layout: %d, %v", n, err)
	}
	bad := map[string]string{
		"empty":           "",
		"only an index":   "3\n",
		"two stripes":     strings.Replace(plainLayout, "lmm_stripe_count:  1", "lmm_stripe_count:  2", 1),
		"data on the MDT": strings.Replace(plainLayout, "raid0", "mdt", 1),
		"released":        strings.Replace(plainLayout, "raid0", "raid0,released", 1),
		"no pattern":      strings.Replace(plainLayout, "lmm_pattern:       raid0\n", "", 1),
		"no offset":       strings.Replace(plainLayout, "lmm_stripe_offset: 3\n", "", 1),
		"negative offset": strings.Replace(plainLayout, "lmm_stripe_offset: 3", "lmm_stripe_offset: -1", 1),
		"offset repeated": plainLayout + "lmm_stripe_offset: 4\n",
		// A composite file: several components, each with its own stripes.
		"composite": "/mnt/testfs/f\n  lcm_layout_gen:    2\n  lcm_mirror_count:  1\n  lcm_entry_count:   2\n" +
			"    lcme_id:             1\n      lmm_stripe_count:  1\n      lmm_pattern:       raid0\n      lmm_stripe_offset: 3\n",
	}
	for name, out := range bad {
		if n, err := ParsePlainLayout([]byte(out)); err == nil {
			t.Errorf("%s: accepted as OST %d", name, n)
		}
	}
}

func TestOneLine(t *testing.T) {
	tests := map[string]string{
		"plain":                 "plain",
		"two\nlines\tand a tab": "two lines and a tab",
		"  padded   text \n":    "padded text",
		"esc\x1b[31mape\x07":    "esc [31mape",
		"bad \xff utf8":         "bad ? utf8",
		"":                      "",
	}
	for in, want := range tests {
		if got := OneLine([]byte(in), 100); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := OneLine([]byte(strings.Repeat("a", 50)), 10); got != "aaaaaaaaaa..." {
		t.Errorf("cut: %q", got)
	}
}

func TestValidKbytes(t *testing.T) {
	for _, in := range []string{"0\n", "123456\n", " 42 "} {
		if !ValidKbytes([]byte(in)) {
			t.Errorf("%q rejected", in)
		}
	}
	for _, in := range []string{"", "\n", "-1\n", "1.5\n", "abc\n", strings.Repeat("9", 30)} {
		if ValidKbytes([]byte(in)) {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSanitizeNID(t *testing.T) {
	tests := map[string]string{
		"10.0.0.2@tcp\n":         "10.0.0.2@tcp",
		"10.0.0.2@o2ib1\nmore\n": "10.0.0.2@o2ib1",
		"[fd00::2]@tcp\n":        "[fd00::2]@tcp",
		"":                       "unknown",
		"bad nid\n":              "unknown",
		"x\"y\n":                 "unknown",
		strings.Repeat("a", 200): "unknown",
	}
	for in, want := range tests {
		if got := SanitizeNID([]byte(in)); got != want {
			t.Errorf("SanitizeNID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPayload(t *testing.T) {
	now := time.Unix(1700000000, 5)
	a := bytes.Repeat([]byte{0xff}, 4096)
	BuildPayload(a, "node-a001", 0xabc, now)
	if !bytes.HasPrefix(a, []byte("lustremesh v1\nclient=node-a001\nseq=abc\ntime=1700000000000000005\n")) {
		t.Errorf("header: %q", a[:80])
	}
	b := make([]byte, 4096)
	BuildPayload(b, "node-a001", 0xabd, now)
	if bytes.Equal(a, b) {
		t.Error("two sequence numbers gave the same page")
	}
	// A page too small for the header still differs by its sequence number.
	c, d := make([]byte, 16), make([]byte, 16)
	BuildPayload(c, "node-a001", 1, now)
	BuildPayload(d, "node-a001", 2, now)
	if bytes.Equal(c, d) {
		t.Error("tiny pages are equal")
	}
	BuildPayload(nil, "x", 1, now) // must not panic
	if !IsPayload(a) || IsPayload(make([]byte, 4096)) || IsPayload(nil) {
		t.Error("IsPayload")
	}
}

func TestClientID(t *testing.T) {
	for _, id := range []string{"node-a001", "n1.zone-a.example", "A_b"} {
		if !ValidClientID(id) {
			t.Errorf("%q rejected", id)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "a b", "a\x00", strings.Repeat("a", 256)} {
		if ValidClientID(id) {
			t.Errorf("%q accepted", id)
		}
	}
	tests := map[string]string{
		"Node-A001": "node-a001",
		"node a001": "node_a001",
		"n1/../etc": "n1_.._etc",
		"":          "",
		".":         "",
		"..":        "",
	}
	for in, want := range tests {
		if got := NormalizeClientID(in); got != want {
			t.Errorf("NormalizeClientID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContended(t *testing.T) {
	c := Config{ContendedRatio: 0.1, ContendedFloorNanos: int64(50 * time.Microsecond)}
	tests := []struct {
		elapsed, delay time.Duration
		want           bool
	}{
		{time.Millisecond, 0, false},
		{time.Millisecond, 40 * time.Microsecond, false}, // under the floor
		{time.Millisecond, 100 * time.Microsecond, true}, // 10% of the duration
		{time.Millisecond, 99 * time.Microsecond, false}, // just under 10%
		{time.Second, 60 * time.Microsecond, false},      // over the floor, negligible share
		{100 * time.Microsecond, 60 * time.Microsecond, true},
	}
	for _, tc := range tests {
		if got := c.Contended(tc.elapsed, tc.delay); got != tc.want {
			t.Errorf("Contended(%v, %v) = %v, want %v", tc.elapsed, tc.delay, got, tc.want)
		}
	}
}

func TestAnswered(t *testing.T) {
	for _, r := range []Result{ResultOK, ResultContended, ResultCached} {
		if !r.Answered() {
			t.Errorf("%s: not answered", r)
		}
	}
	for _, r := range []Result{ResultTimeout, ResultDisconnected, ResultInactive, ResultError, ResultMismatch, ResultStuck, ResultUnprepared, ResultSkipped} {
		if r.Answered() {
			t.Errorf("%s: answered", r)
		}
	}
}

func FuzzParsers(f *testing.F) {
	f.Add([]byte("ost_statfs 17 samples [usec] 80 400 2000\n"))
	f.Add([]byte("current_state: FULL\n"))
	f.Add([]byte("123456 7890 12\n"))
	f.Add([]byte(plainLayout))
	f.Add([]byte("10.0.0.2@tcp\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		ParseStatCount(data, "ost_statfs")
		if s := ParseImportState(data); s != StateUnknown && !validState(s) {
			t.Fatalf("state %q", s)
		}
		if d, ok := ParseRunDelay(data); ok && d < 0 {
			t.Fatalf("negative run delay %v", d)
		}
		_, _ = ParsePlainLayout(data)
		if line := OneLine(data, 64); len(line) > 64+3 || strings.ContainsAny(line, "\n\r\t\x1b\x00") {
			t.Fatalf("OneLine gave %q", line)
		}
		ValidKbytes(data)
		nid := SanitizeNID(data)
		if strings.ContainsAny(nid, " \t\n\"\\{}") {
			t.Fatalf("NID %q", nid)
		}
		if id := NormalizeClientID(string(data)); strings.ContainsAny(id, "/\x00 ") || id == "." || id == ".." || len(id) > 255 {
			t.Fatalf("client ID %q", id)
		}
	})
}
