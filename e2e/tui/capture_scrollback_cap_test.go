package tuie2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// capture-pane --scrollback with no --lines keeps the last 2000 lines and says
// how many it left out: one line on stderr, and lines_omitted in the JSON.
// --lines 0 gives the whole history.
//
// How this could pass wrongly, written down first:
//   - The pane could hold fewer than 2000 lines, so nothing is cut and a
//     capture with no cap at all looks capped. The pane prints 2500 numbered
//     lines, and the --lines 0 capture must hold every one of them.
//   - The capped capture could keep the wrong end. It must hold CAP2500 and
//     not CAP1.
//   - The note could be printed on stdout, where a script reading the
//     capture would take it for pane text. stdout and stderr are read apart.
//   - lines_omitted could be a guess. It must equal the line count of the
//     whole history minus 2000.
//   - The note could be printed always. The --lines 0 and --lines 50 captures
//     must print nothing on stderr.
//
// Negative control: on origin/main the capped capture holds CAP1 and prints
// no note. With the CLI default cut (capped never set), the same.
func TestCaptureScrollbackCapsAtTwoThousandLines(t *testing.T) {
	base := t.TempDir()
	killDaemon(t, base)
	const sess = "capcap"
	if out, err := tuiosCLI(t, base, "new", sess, "--detach"); err != nil {
		t.Fatalf("create the session: %v: %s", err, out)
	}
	if out, err := tuiosCLI(t, base, "send-text", "-s", sess, "-w", "0", "printf 'CAP%d\\n' $(seq 1 2500)\n"); err != nil {
		t.Fatalf("send-text: %v: %s", err, out)
	}
	if out, err := tuiosCLI(t, base, "wait-for", "window-output", "-s", sess, "-w", "0", "--pattern", `CAP2500\b`, "--timeout", "20000"); err != nil {
		t.Fatalf("the pane never printed its last line: %v: %s", err, out)
	}

	run := func(args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(tuiosBin, append([]string{"capture-pane", "-s", sess, "-w", "0", "--scrollback"}, args...)...)
		cmd.Env = cliEnv(base)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		if err != nil {
			t.Fatalf("capture-pane %v: %v: %s", args, err, stderr.String())
		}
		return string(stdout), stderr.String()
	}
	has := func(content, line string) bool {
		return strings.Contains("\n"+content, "\n"+line+"\n")
	}
	nonBlank := func(content string) int {
		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		n := len(lines)
		for n > 0 && strings.TrimSpace(lines[n-1]) == "" {
			n--
		}
		return n
	}

	// Positive half first: --lines 0 is the whole history.
	full, fullErr := run("--lines", "0")
	for _, want := range []string{"CAP1", "CAP1250", "CAP2500"} {
		if !has(full, want) {
			t.Fatalf("ASSERTION: --lines 0 left out %s, want the whole history", want)
		}
	}
	if fullErr != "" {
		t.Fatalf("ASSERTION: --lines 0 printed a note on stderr: %q", fullErr)
	}
	total := nonBlank(full)

	capped, cappedErr := run()
	if has(capped, "CAP1") || !has(capped, "CAP2500") {
		t.Fatalf("ASSERTION: --scrollback with no --lines did not keep only the newest lines: CAP1 present=%v, CAP2500 present=%v, %d lines",
			has(capped, "CAP1"), has(capped, "CAP2500"), nonBlank(capped))
	}
	if n := nonBlank(capped); n != 2000 {
		t.Fatalf("ASSERTION: the capped capture holds %d lines, want 2000", n)
	}
	if !strings.Contains(cappedErr, "2000") || !strings.Contains(cappedErr, "--lines 0") ||
		!strings.Contains(cappedErr, itoa(total-2000)) {
		t.Fatalf("ASSERTION: the note on stderr does not say the cap, the %d lines left out, and --lines 0: %q", total-2000, cappedErr)
	}
	if strings.Contains(capped, "note:") {
		t.Fatalf("ASSERTION: the note reached stdout")
	}

	jsonOut, jsonErr := run("--json")
	var res struct {
		Content      string `json:"content"`
		LinesOmitted *int   `json:"lines_omitted"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil {
		t.Fatalf("decode the JSON capture: %v: %.200s", err, jsonOut)
	}
	if res.LinesOmitted == nil || *res.LinesOmitted != total-2000 {
		t.Fatalf("ASSERTION: lines_omitted is %v, want %d", res.LinesOmitted, total-2000)
	}
	if jsonErr != "" {
		t.Fatalf("ASSERTION: --json printed a note on stderr: %q", jsonErr)
	}

	small, smallErr := run("--lines", "50")
	if n := nonBlank(small); n != 50 || smallErr != "" {
		t.Fatalf("ASSERTION: --lines 50 gave %d lines and stderr %q, want 50 lines and no note", n, smallErr)
	}

	t.Logf("whole history %d lines, capped capture %d lines, note: %s", total, nonBlank(capped), strings.TrimSpace(cappedErr))
}
