// SPDX-License-Identifier: Apache-2.0

// Package target names the Lustre targets a client is connected to.
//
// A client has one import device per target, listed as a directory of
// /sys/fs/lustre/osc (OSTs) and /sys/fs/lustre/mdc (MDTs). This package only
// parses and selects names: it never reads the file system, because every
// access to Lustre belongs to a worker process (docs/design.md §6).
package target

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind is the type of a target.
type Kind string

const (
	KindOST Kind = "ost"
	KindMDT Kind = "mdt"
)

// Device is the client-side import of one target.
type Device struct {
	// Name is the directory name, such as "lustre-OST0003-osc-ffff8f0b3c5e2000".
	Name string `json:"name"`
	// FS is the file system name ("lustre").
	FS string `json:"fs"`
	// Kind tells an OST from an MDT.
	Kind Kind `json:"kind"`
	// Index is the target index.
	Index uint32 `json:"index"`
	// IndexHex is the index as the name spells it ("0003"); it names the
	// target's directory under the probe root.
	IndexHex string `json:"index_hex"`
	// Instance identifies the mount the import belongs to ("ffff8f0b3c5e2000").
	Instance string `json:"instance"`
	// ServerNID is the NID of the server the import is connected to, or
	// "unknown" when it could not be read.
	ServerNID string `json:"server_nid"`
}

// UnknownNID stands for a server NID that could not be read.
const UnknownNID = "unknown"

// Label is the target's name without the client-side suffix ("lustre-OST0003").
func (d Device) Label() string {
	tag := "OST"
	if d.Kind == KindMDT {
		tag = "MDT"
	}
	return d.FS + "-" + tag + d.IndexHex
}

// ClientType is the name of the client device type: "osc" or "mdc".
func (d Device) ClientType() string {
	if d.Kind == KindMDT {
		return "mdc"
	}
	return "osc"
}

// maxNameLen bounds a device name; Linux file names are at most 255 bytes.
const maxNameLen = 255

// ParseDeviceName parses the name of a directory of /sys/fs/lustre/osc or
// /sys/fs/lustre/mdc. It accepts only client imports, whose last part is the
// hexadecimal instance of a mount: the imports a server holds towards other
// targets end in a target name ("-osc-MDT0000") and are rejected.
func ParseDeviceName(name string) (Device, error) {
	if len(name) == 0 || len(name) > maxNameLen {
		return Device{}, fmt.Errorf("device name of %d bytes", len(name))
	}
	// <fs>-<OST|MDT><hex index>-<osc|mdc>-<hex instance>, read from the right
	// because the file system name may itself contain hyphens.
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return Device{}, fmt.Errorf("%q: no instance", name)
	}
	rest, instance := name[:i], name[i+1:]
	if !isLowerHex(instance) || len(instance) < 8 || len(instance) > 16 {
		return Device{}, fmt.Errorf("%q: %q is not a mount instance", name, instance)
	}
	i = strings.LastIndexByte(rest, '-')
	if i < 0 {
		return Device{}, fmt.Errorf("%q: no device type", name)
	}
	rest, clientType := rest[:i], rest[i+1:]
	i = strings.LastIndexByte(rest, '-')
	if i < 0 {
		return Device{}, fmt.Errorf("%q: no target", name)
	}
	fs, tgt := rest[:i], rest[i+1:]
	if len(tgt) < 3 {
		return Device{}, fmt.Errorf("%q: no target", name)
	}
	var kind Kind
	switch {
	case tgt[:3] == "OST" && clientType == "osc":
		kind = KindOST
	case tgt[:3] == "MDT" && clientType == "mdc":
		kind = KindMDT
	default:
		return Device{}, fmt.Errorf("%q: target %q does not match device type %q", name, tgt, clientType)
	}
	hex := tgt[3:]
	if !isLowerHex(hex) || len(hex) < 4 || len(hex) > 8 {
		return Device{}, fmt.Errorf("%q: %q is not a target index", name, hex)
	}
	index, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return Device{}, fmt.Errorf("%q: %q is not a target index", name, hex)
	}
	if !validFSName(fs) {
		return Device{}, fmt.Errorf("%q: %q is not a file system name", name, fs)
	}
	return Device{
		Name:      name,
		FS:        fs,
		Kind:      kind,
		Index:     uint32(index),
		IndexHex:  hex,
		Instance:  instance,
		ServerNID: UnknownNID,
	}, nil
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validFSName accepts the characters of a Lustre file system name. Lustre
// itself limits the name to 8 characters; the bound here only keeps labels
// short.
func validFSName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// ErrNoDevices reports that the selection is empty.
var ErrNoDevices = errors.New("no Lustre client device found")

// Select keeps the devices of one file system and one mount, MDTs first and
// then OSTs, each in index order.
//
// fsname and instance may be empty when they are not needed to tell devices
// apart. Select refuses to guess: several file systems without fsname, or one
// target imported by several mounts without instance, are errors.
func Select(devs []Device, fsname, instance string) ([]Device, error) {
	fsSeen := map[string]bool{}
	for _, d := range devs {
		fsSeen[d.FS] = true
	}
	if fsname == "" {
		switch len(fsSeen) {
		case 0:
			return nil, ErrNoDevices
		case 1:
			for fs := range fsSeen {
				fsname = fs
			}
		default:
			return nil, fmt.Errorf("several file systems are mounted (%s): choose one with --fsname", joinKeys(fsSeen))
		}
	}
	var out []Device
	instSeen := map[string]bool{}
	for _, d := range devs {
		if d.FS != fsname {
			continue
		}
		instSeen[d.Instance] = true
		if instance != "" && d.Instance != instance {
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		if instance != "" && len(instSeen) > 0 {
			return nil, fmt.Errorf("file system %q has no mount instance %q (found %s)", fsname, instance, joinKeys(instSeen))
		}
		if len(fsSeen) > 0 {
			return nil, fmt.Errorf("no device of file system %q (found %s)", fsname, joinKeys(fsSeen))
		}
		return nil, ErrNoDevices
	}
	if instance == "" && len(instSeen) > 1 {
		return nil, fmt.Errorf("file system %q is mounted several times (instances %s): choose one with --instance", fsname, joinKeys(instSeen))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == KindMDT
		}
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Name < out[j].Name
	})
	for i := 1; i < len(out); i++ {
		if out[i].Kind == out[i-1].Kind && out[i].Index == out[i-1].Index {
			return nil, fmt.Errorf("target %s is listed twice (%s, %s)", out[i].Label(), out[i-1].Name, out[i].Name)
		}
	}
	return out, nil
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
