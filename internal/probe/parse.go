// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// StateUnknown stands for an import state that could not be read.
const StateUnknown = "unknown"

// ParseStatCount returns the event count of one line of a Lustre "stats"
// file, whose lines are "<name> <count> samples [<unit>] ...". ok is false
// when the file has no such line: the caller then knows nothing, not zero.
func ParseStatCount(data []byte, name string) (count uint64, ok bool) {
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		fields := bytes.Fields(line)
		if len(fields) < 2 || string(fields[0]) != name {
			continue
		}
		n, err := strconv.ParseUint(string(fields[1]), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// ParseImportState returns the current state of an import ("FULL", "IDLE",
// "DISCONN", ...) from its "state" file, whose first line is
// "current_state: <STATE>". It returns StateUnknown for anything else.
func ParseImportState(data []byte) string {
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		rest, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("current_state:"))
		if !found {
			continue
		}
		state := string(bytes.TrimSpace(rest))
		if !validState(state) {
			return StateUnknown
		}
		return state
	}
	return StateUnknown
}

func validState(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && c != '_' {
			return false
		}
	}
	return true
}

// ParseRunDelay returns the cumulative run-queue wait of a thread from its
// /proc/thread-self/schedstat: "<on-cpu ns> <run-queue ns> <timeslices>".
// ok is false when the kernel does not account it (all fields zero) or the
// content is not understood.
func ParseRunDelay(data []byte) (delay time.Duration, ok bool) {
	fields := bytes.Fields(data)
	if len(fields) != 3 {
		return 0, false
	}
	var v [3]uint64
	for i, f := range fields {
		n, err := strconv.ParseUint(string(f), 10, 64)
		if err != nil {
			return 0, false
		}
		v[i] = n
	}
	if v[0] == 0 && v[1] == 0 && v[2] == 0 {
		return 0, false
	}
	if v[1] > math.MaxInt64 {
		return 0, false
	}
	return time.Duration(v[1]), true //nolint:gosec // bounded just above
}

// ParsePlainLayout reads the output of "lfs getstripe <file>" and returns
// the OST index of a file with a plain layout of one stripe:
//
//	lmm_stripe_count:  1
//	lmm_pattern:       raid0
//	lmm_stripe_offset: 3
//
// Everything else is an error, on purpose: for a composite layout (PFL,
// mirrors) or data on the MDT, no single index says where the first page
// of the file lives.
func ParsePlainLayout(out []byte) (uint32, error) {
	var count, pattern, offset string
	seen := map[string]bool{}
	for len(out) > 0 {
		var line []byte
		line, out, _ = bytes.Cut(out, []byte{'\n'})
		k, v, found := bytes.Cut(bytes.TrimSpace(line), []byte{':'})
		if !found {
			continue
		}
		key, value := string(k), string(bytes.TrimSpace(v))
		if strings.HasPrefix(key, "lcm_") || strings.HasPrefix(key, "lcme_") {
			return 0, errors.New("the file has a composite layout, not a plain one")
		}
		switch key {
		case "lmm_stripe_count":
			count = value
		case "lmm_pattern":
			pattern = value
		case "lmm_stripe_offset":
			offset = value
		default:
			continue
		}
		if seen[key] {
			return 0, fmt.Errorf("lfs printed %s twice", key)
		}
		seen[key] = true
	}
	for _, key := range []string{"lmm_stripe_count", "lmm_pattern", "lmm_stripe_offset"} {
		if !seen[key] {
			return 0, fmt.Errorf("lfs printed no %s", key)
		}
	}
	if count != "1" {
		return 0, fmt.Errorf("the file has %s stripes, not 1", OneLine([]byte(count), 20))
	}
	if pattern != "raid0" {
		return 0, fmt.Errorf("the layout pattern is %q, not raid0", OneLine([]byte(pattern), 40))
	}
	n, err := strconv.ParseUint(offset, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("lfs printed the stripe offset %q", OneLine([]byte(offset), 40))
	}
	return uint32(n), nil
}

// OneLine makes text from elsewhere (a command's output) safe to show in a
// one-line report: control characters and line breaks become spaces, runs of
// spaces collapse, and the result is cut to max bytes.
func OneLine(b []byte, max int) string {
	var sb strings.Builder
	space := false
	for _, r := range strings.ToValidUTF8(string(b), "?") {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' {
			space = sb.Len() > 0
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		sb.WriteRune(r)
		if sb.Len() >= max {
			return sb.String()[:max] + "..."
		}
	}
	return sb.String()
}

// ValidKbytes checks that the bytes read from a kbytesfree file are a decimal
// number, so that a path pointing somewhere else is not taken for an answer.
func ValidKbytes(data []byte) bool {
	s := bytes.TrimSpace(data)
	if len(s) == 0 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SanitizeNID returns the first line of a conn_uuid file as a NID, or
// UnknownNID's value "unknown" when it does not look like one.
func SanitizeNID(data []byte) string {
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	s := string(bytes.TrimSpace(line))
	if s == "" || len(s) > 128 {
		return "unknown"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == ':', c == '@', c == '-', c == '_', c == '[', c == ']':
		default:
			return "unknown"
		}
	}
	return s
}

// payloadMagic starts the page a data probe writes.
const payloadMagic = "lustremesh v1\n"

// BuildPayload fills page with the content of one data probe: a readable
// header naming the client, a sequence number and the time, then zeros.
// The read that follows must return exactly these bytes.
func BuildPayload(page []byte, clientID string, seq uint64, now time.Time) {
	clear(page)
	n := copy(page, payloadMagic)
	n += copy(page[n:], "client="+clientID+"\n")
	n += copy(page[n:], "seq="+strconv.FormatUint(seq, 16)+"\n")
	copy(page[n:], "time="+strconv.FormatInt(now.UnixNano(), 10)+"\n")
	// The sequence number again in binary in the last bytes of the page, so
	// that a header cut short by a tiny page still differs between probes.
	if len(page) >= 8 {
		binary.LittleEndian.PutUint64(page[len(page)-8:], seq)
	}
}

// IsPayload reports whether a page starts like one a data probe wrote.
func IsPayload(page []byte) bool {
	return bytes.HasPrefix(page, []byte(payloadMagic))
}

// ValidClientID reports whether id can name a client's file under the probe
// root: one path component of letters, digits, '.', '_' and '-'.
func ValidClientID(id string) bool {
	if id == "" || len(id) > 255 || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// NormalizeClientID turns a host name into a client ID: lower case, with
// every other character replaced by '_'. It returns "" when nothing usable
// is left.
func NormalizeClientID(host string) string {
	b := make([]byte, 0, len(host))
	for i := 0; i < len(host) && len(b) < 255; i++ {
		c := host[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	id := string(b)
	if !ValidClientID(id) {
		return ""
	}
	return id
}
