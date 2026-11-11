// SPDX-License-Identifier: Apache-2.0

package target

import (
	"strings"
	"testing"
)

const inst = "ffff8f0b3c5e2000"

func TestParseDeviceName(t *testing.T) {
	tests := []struct {
		name string
		want Device
	}{
		{"testfs-OST0003-osc-" + inst, Device{FS: "testfs", Kind: KindOST, Index: 3, IndexHex: "0003", Instance: inst}},
		{"testfs-MDT0000-mdc-" + inst, Device{FS: "testfs", Kind: KindMDT, Index: 0, IndexHex: "0000", Instance: inst}},
		{"zone-a-OST00ff-osc-" + inst, Device{FS: "zone-a", Kind: KindOST, Index: 255, IndexHex: "00ff", Instance: inst}},
		{"fs_1-OST10000-osc-0123abcd", Device{FS: "fs_1", Kind: KindOST, Index: 0x10000, IndexHex: "10000", Instance: "0123abcd"}},
	}
	for _, tc := range tests {
		got, err := ParseDeviceName(tc.name)
		if err != nil {
			t.Errorf("ParseDeviceName(%q): %v", tc.name, err)
			continue
		}
		tc.want.Name = tc.name
		tc.want.ServerNID = UnknownNID
		if got != tc.want {
			t.Errorf("ParseDeviceName(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseDeviceNameRejects(t *testing.T) {
	for _, name := range []string{
		"",
		"testfs",
		"testfs-OST0000-osc-MDT0000",      // a server's import of an OST
		"testfs-OST0000-mdc-" + inst,      // target and device type disagree
		"testfs-MDT0000-osc-" + inst,      //
		"testfs-OST000-osc-" + inst,       // index too short
		"testfs-OST00000000f-osc-" + inst, // index too long
		"testfs-OST00FF-osc-" + inst,      // upper-case index
		"testfs-OST0000-osc-ffff",         // instance too short
		"testfs-OST0000-osc-FFFF8F0B3C5E2000",
		"-OST0000-osc-" + inst,         // no file system
		"a/b-OST0000-osc-" + inst,      // path separator
		"..-OST0000-osc-" + inst + "/", //
		"te st-OST0000-osc-" + inst,
		"testfs-MGS-mgc-" + inst,
		strings.Repeat("a", 300),
	} {
		if d, err := ParseDeviceName(name); err == nil {
			t.Errorf("ParseDeviceName(%q) = %+v, want an error", name, d)
		}
	}
}

func TestLabelAndClientType(t *testing.T) {
	d, err := ParseDeviceName("testfs-OST000a-osc-" + inst)
	if err != nil {
		t.Fatal(err)
	}
	if d.Label() != "testfs-OST000a" || d.ClientType() != "osc" {
		t.Errorf("got %q %q", d.Label(), d.ClientType())
	}
	m, err := ParseDeviceName("testfs-MDT0001-mdc-" + inst)
	if err != nil {
		t.Fatal(err)
	}
	if m.Label() != "testfs-MDT0001" || m.ClientType() != "mdc" {
		t.Errorf("got %q %q", m.Label(), m.ClientType())
	}
}

func mustParse(t *testing.T, names ...string) []Device {
	t.Helper()
	var out []Device
	for _, n := range names {
		d, err := ParseDeviceName(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func labels(devs []Device) string {
	var s []string
	for _, d := range devs {
		s = append(s, d.Label())
	}
	return strings.Join(s, " ")
}

func TestSelectOrdersMDTsFirst(t *testing.T) {
	devs := mustParse(t,
		"testfs-OST0001-osc-"+inst, "testfs-OST0000-osc-"+inst,
		"testfs-MDT0001-mdc-"+inst, "testfs-MDT0000-mdc-"+inst)
	got, err := Select(devs, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "testfs-MDT0000 testfs-MDT0001 testfs-OST0000 testfs-OST0001"; labels(got) != want {
		t.Errorf("got %q, want %q", labels(got), want)
	}
}

func TestSelectRefusesToGuess(t *testing.T) {
	other := "ffff8f0b3c5e3000"
	twoFS := mustParse(t, "alpha-OST0000-osc-"+inst, "beta-OST0000-osc-"+other)
	if _, err := Select(twoFS, "", ""); err == nil || !strings.Contains(err.Error(), "--fsname") {
		t.Errorf("two file systems: err = %v", err)
	}
	got, err := Select(twoFS, "beta", "")
	if err != nil || labels(got) != "beta-OST0000" {
		t.Errorf("--fsname beta: %q, %v", labels(got), err)
	}

	twoMounts := mustParse(t, "alpha-OST0000-osc-"+inst, "alpha-OST0000-osc-"+other)
	if _, err := Select(twoMounts, "", ""); err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Errorf("two mounts: err = %v", err)
	}
	got, err = Select(twoMounts, "", other)
	if err != nil || len(got) != 1 || got[0].Instance != other {
		t.Errorf("--instance: %+v, %v", got, err)
	}

	if _, err := Select(twoMounts, "gamma", ""); err == nil {
		t.Error("unknown fsname: no error")
	}
	if _, err := Select(twoMounts, "", "0123456789abcdef"); err == nil {
		t.Error("unknown instance: no error")
	}
	if _, err := Select(nil, "", ""); err == nil {
		t.Error("no device: no error")
	}
}

func TestSelectRejectsDuplicateIndex(t *testing.T) {
	// The same index spelled with different widths would otherwise be two
	// targets sharing one lock and one directory.
	devs := mustParse(t, "alpha-OST0001-osc-"+inst, "alpha-OST00001-osc-"+inst)
	if _, err := Select(devs, "", ""); err == nil {
		t.Error("duplicate index: no error")
	}
}

func FuzzParseDeviceName(f *testing.F) {
	f.Add("testfs-OST0003-osc-" + inst)
	f.Add("testfs-MDT0000-mdc-" + inst)
	f.Add("a-b-OST0000-osc-MDT0000")
	f.Add("")
	f.Fuzz(func(t *testing.T, name string) {
		d, err := ParseDeviceName(name)
		if err != nil {
			return
		}
		// An accepted name is one path component, and its parts rebuild it.
		if strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
			t.Fatalf("accepted %q", name)
		}
		if got := d.Label() + "-" + d.ClientType() + "-" + d.Instance; got != name {
			t.Fatalf("%q rebuilt as %q", name, got)
		}
		if strings.ContainsAny(d.Label(), "/\x00") {
			t.Fatalf("label %q", d.Label())
		}
	})
}
