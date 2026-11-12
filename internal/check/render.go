// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuki/lustremesh/internal/probe"
)

// WriteJSON writes the report as one indented JSON document.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// columns are the steps shown as columns of the text report; the others
// (verify, close) appear in the notes when they are not ok.
var columns = []probe.Step{probe.StepRPC, probe.StepOpen, probe.StepWrite, probe.StepRead}

// WriteText writes the report as a table, one line per target.
func WriteText(w io.Writer, r Report) error {
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tSERVER\tSTATE\tRPC\tOPEN\tWRITE\tREAD\tNOTE")
	bad := 0
	for _, t := range r.Targets {
		byStep := map[probe.Step]probe.StepResult{}
		failed := false
		for _, s := range t.Steps {
			byStep[s.Step] = s
			if !s.Result.Answered() {
				failed = true
			}
		}
		if failed {
			bad++
		}
		cells := []string{t.Target, probe.OneLine([]byte(t.ServerNID), 128), probe.OneLine([]byte(t.State), 32)}
		for _, c := range columns {
			s, ok := byStep[c]
			if !ok {
				cells = append(cells, "-")
				continue
			}
			cells = append(cells, cell(s))
		}
		cells = append(cells, strings.Join(notes(t), "; "))
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// The table pads an empty last cell: drop the trailing spaces.
	for _, line := range strings.Split(strings.TrimRight(table.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\n%d targets of %s, %d with a step that got no answer", len(r.Targets), r.FS, bad)
	if r.Blocked > 0 {
		fmt.Fprintf(w, "; %d worker(s) still blocked in a probe and left behind", r.Blocked)
	}
	fmt.Fprintln(w)
	if len(r.StuckTargets) > 0 {
		fmt.Fprintf(w, "%d target(s) with a probe still blocked, from this run or an earlier one: %s\n", len(r.StuckTargets), strings.Join(r.StuckTargets, ", "))
	}
	if n := unverifiedRPCs(r); n > 0 {
		fmt.Fprintf(w, "%d rpc probe(s) ok without proof that an RPC was sent: the RPC counters (debugfs) are not readable\n", n)
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "not client imports, ignored: %s\n", strings.Join(r.Skipped, ", "))
	}
	return nil
}

func cell(s probe.StepResult) string {
	out := string(s.Result)
	if s.Errno != "" {
		out += " " + s.Errno
	}
	if s.Nanos != nil {
		out += " " + formatDuration(time.Duration(*s.Nanos))
	}
	return out
}

// notes explains what the cells cannot: why a step was not ok, and a stall
// that ended.
func notes(t Target) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range t.Steps {
		if s.Result == probe.ResultOK || s.Detail == "" {
			continue
		}
		// A detail may quote a command's output: keep it on one line.
		detail := probe.OneLine([]byte(s.Detail), 300)
		n := fmt.Sprintf("%s: %s", s.Step, detail)
		if s.Result == probe.ResultSkipped {
			// Several steps are usually skipped for one reason.
			n = "skipped: " + detail
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if t.Stall != nil {
		out = append(out, fmt.Sprintf("%s returned after %s", t.Stall.Step, formatDuration(time.Duration(t.Stall.Nanos))))
	}
	return out
}

// unverifiedRPCs counts the rpc probes that succeeded without the RPC
// counters telling whether the client's statfs cache answered.
func unverifiedRPCs(r Report) int {
	n := 0
	for _, t := range r.Targets {
		for _, s := range t.Steps {
			if s.Step == probe.StepRPC && s.Result == probe.ResultOK && s.RPCSent == probe.RPCSentUnknown {
				n++
			}
		}
	}
	return n
}

// formatDuration keeps three significant digits.
func formatDuration(d time.Duration) string {
	unit := time.Duration(1)
	for d >= 1000*unit {
		unit *= 10
	}
	return d.Round(unit).String()
}
