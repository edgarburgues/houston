package module

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperCmd builds an argv that re-execs this test binary into
// TestHelperProcess with a mode — the cross-platform stand-in for a handler
// (no pwsh/sh scripts in CI, identical behavior on Windows and POSIX).
func helperCmd(mode string, extra ...string) []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return append([]string{exe, "-test.run=^TestHelperProcess$", "--", mode}, extra...)
}

func testModule(t *testing.T, name string) Module {
	t.Helper()
	return Module{Entry: Entry{Name: name, Enabled: true}, Manifest: Manifest{API: 1, Name: name}, Dir: t.TempDir()}
}

// TestHelperProcess is not a test: it is the re-exec target for runner tests.
// It only acts when invoked with a "--"-separated mode.
func TestHelperProcess(t *testing.T) {
	args := os.Args
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) {
		return
	}
	mode := args[sep+1]
	extra := args[sep+2:]
	defer os.Exit(0)
	switch mode {
	case "echo-stdin":
		io.Copy(os.Stdout, os.Stdin)
	case "json":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"status":"done","refresh":true}`)
	case "empty":
		io.Copy(io.Discard, os.Stdin)
	case "action-notice":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"notice":"from the notice"}`)
	case "action-status-and-notice":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"status":"the status","notice":"ignored"}`)
	case "bom":
		io.Copy(io.Discard, os.Stdin)
		os.Stdout.Write([]byte{0xEF, 0xBB, 0xBF})
		fmt.Print(`{"status":"bom"}`)
	case "garbage":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print("Loading module...")
	case "big":
		io.Copy(io.Discard, os.Stdin)
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 40; i++ { // 2.5 MiB
			os.Stdout.Write(chunk)
		}
	case "exit1-json":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"status":"half-truth"}`)
		os.Exit(1)
	case "stderr-fail":
		io.Copy(io.Discard, os.Stdin)
		filler := strings.Repeat("f", 60<<10)
		fmt.Fprint(os.Stderr, filler+"TAIL-MARKER")
		os.Exit(1)
	case "hang":
		time.Sleep(60 * time.Second)
	case "sleep-json":
		ms, _ := strconv.Atoi(extra[0])
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Print(`{}`)
	case "spew-then-drain":
		// Writes well past the pipe buffer BEFORE reading stdin — the
		// mutual-wait deadlock shape. Spaces, so the decoder's TrimLeft
		// still finds the JSON.
		os.Stdout.Write(bytes.Repeat([]byte(" "), 100<<10))
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"status":"survived"}`)
	case "grandchild":
		// Spawn a silent long-lived child that inherits our stdout pipe,
		// then exit: without WaitDelay the runner would block on the pipe
		// until the grandchild dies.
		var c *exec.Cmd
		if runtime.GOOS == "windows" {
			c = exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >NUL")
		} else {
			c = exec.Command("sh", "-c", "sleep 30")
		}
		c.Stdout = os.Stdout
		// Not our cwd (the module temp dir): a lingering grandchild must
		// not hold the test TempDir open on Windows.
		c.Dir = os.TempDir()
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(`{"status":"parent-done"}`)
	case "env":
		io.Copy(io.Discard, os.Stdin)
		out := map[string]string{
			"claude": os.Getenv("CLAUDE_CONFIG_DIR"),
			"api":    os.Getenv("HOUSTON_API"),
			"event":  os.Getenv("HOUSTON_EVENT"),
			"module": os.Getenv("HOUSTON_MODULE"),
			"dir":    os.Getenv("HOUSTON_MODULE_DIR"),
			"store":  os.Getenv("HOUSTON_STORE_DIR"),
		}
		json.NewEncoder(os.Stdout).Encode(out)
	case "transform-a":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"patches":[{"key":"k1","title":"TA","badge":"A"},{"key":"k1","badge":"dup-ignored"},{"key":"nope","hide":true}],"notice":"hello"}`)
	case "transform-b":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"patches":[{"key":"k1","badge":"B","hide":true}]}`)
	case "transform-dirty":
		io.Copy(io.Discard, os.Stdin)
		long := strings.Repeat("t", 400)
		fmt.Printf(`{"patches":[{"key":"k1","title":%q,"badge":"\u001b[31mRED\u001b[0m-and-much-more-text"}]}`, long)
	case "preview":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"sections":[{"title":"S1","body":"line1\n\tindented"},{"title":"S2","body":"b"},{"title":"S3","body":"c"},{"title":"S4","body":"never"}]}`)
	case "seg-mark":
		// Drops a unique marker file in extra[0] so segcache tests can count
		// execs, then replies with extra[1] as the segment text.
		io.Copy(io.Discard, os.Stdin)
		f, err := os.CreateTemp(extra[0], "exec-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		f.Close()
		fmt.Printf(`{"text":%q}`, extra[1])
	case "view":
		io.Copy(io.Discard, os.Stdin)
		fmt.Print(`{"title":"My Page","body":"line1\nline2 with \u001b[31mansi\u001b[0m"}`)
	case "seg-sleep":
		ms, _ := strconv.Atoi(extra[0])
		io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Printf(`{"text":%q}`, extra[1])
	}
}

func TestInvokeRoundTrip(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "roundtrip")
	raw, err := Invoke(context.Background(), m, helperCmd("json"), NewEnvelope(EventAction, m, nil), CapReply, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var rep actionReplyWire
	if err := DecodeReply(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "done" || !rep.Refresh {
		t.Fatalf("got %+v", rep)
	}
}

func TestInvokeEnvContract(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "should-be-stripped")
	m := testModule(t, "envmod")
	raw, err := Invoke(context.Background(), m, helperCmd("env"), NewEnvelope(EventTransform, m, nil), CapReply, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := DecodeReply(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["claude"] != "" {
		t.Errorf("CLAUDE_CONFIG_DIR leaked into the handler: %q", got["claude"])
	}
	want := map[string]string{"api": "1", "event": EventTransform, "module": "envmod", "dir": m.Dir}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	if got["store"] == "" {
		t.Error("HOUSTON_STORE_DIR not set")
	}
}

func TestInvokeEnvelopeReachesStdin(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "echo")
	m.Settings = json.RawMessage(`{"jiraUrl":"https://example"}`)
	env := NewEnvelope(EventPreview, m, PreviewPayload{Mission: MissionRow{Key: "p/i", Title: "T"}})
	raw, err := Invoke(context.Background(), m, helperCmd("echo-stdin"), env, CapReply, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("stdin was not the envelope: %v", err)
	}
	if back.API != 1 || back.Event != EventPreview || back.Module != "echo" {
		t.Fatalf("envelope fields: %+v", back)
	}
	if string(back.Settings) != `{"jiraUrl":"https://example"}` {
		t.Fatalf("settings passthrough: %s", back.Settings)
	}
	if back.Houston.OS != runtime.GOOS || back.Houston.StoreDir == "" {
		t.Fatalf("houston info: %+v", back.Houston)
	}
}

func TestInvokeTimeoutKillsTree(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "hangmod")
	start := time.Now()
	_, err := Invoke(context.Background(), m, helperCmd("hang"), NewEnvelope(EventAction, m, nil), CapReply, 500*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("kill took %s", elapsed)
	}
	var ee *ExecError
	if !errors.As(err, &ee) || !ee.Timeout {
		t.Fatalf("want timeout ExecError, got %v", err)
	}
}

func TestInvokeGrandchildDoesNotHangWait(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "grandchild")
	start := time.Now()
	raw, err := Invoke(context.Background(), m, helperCmd("grandchild"), NewEnvelope(EventAction, m, nil), CapReply, 25*time.Second)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("lingering grandchild held Invoke for %s", elapsed)
	}
	if err != nil {
		t.Fatalf("handler exited clean, want success: %v", err)
	}
	var rep actionReplyWire
	if err := DecodeReply(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "parent-done" {
		t.Fatalf("got %+v", rep)
	}
}

func TestInvokePipeDeadlock(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "spew")
	// >1 MiB envelope against a handler that writes 100 KB before reading
	// stdin: only concurrent stdin/stdout pumps complete this.
	payload := struct {
		Filler string `json:"filler"`
	}{Filler: strings.Repeat("p", 1<<21)}
	raw, err := Invoke(context.Background(), m, helperCmd("spew-then-drain"), NewEnvelope(EventTransform, m, payload), CapReply, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var rep actionReplyWire
	if err := DecodeReply(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "survived" {
		t.Fatalf("got %+v", rep)
	}
}

func TestInvokeStdoutCap(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "bigmod")
	_, err := Invoke(context.Background(), m, helperCmd("big"), NewEnvelope(EventAction, m, nil), CapReply, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("want cap failure, got %v", err)
	}
}

func TestInvokeNonzeroExitDiscardsJSON(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "exit1")
	raw, err := Invoke(context.Background(), m, helperCmd("exit1-json"), NewEnvelope(EventAction, m, nil), CapReply, 20*time.Second)
	if err == nil {
		t.Fatalf("nonzero exit must fail, got %q", raw)
	}
	var ee *ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 1 {
		t.Fatalf("want exit code 1, got %v", err)
	}
}

func TestInvokeStderrTailAndLog(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "stderrmod")
	_, err := Invoke(context.Background(), m, helperCmd("stderr-fail"), NewEnvelope(EventAction, m, nil), CapReply, 20*time.Second)
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("want ExecError, got %v", err)
	}
	if len(ee.Stderr) > stderrTail {
		t.Fatalf("stderr tail %d > %d", len(ee.Stderr), stderrTail)
	}
	if !bytes.HasSuffix(ee.Stderr, []byte("TAIL-MARKER")) {
		t.Fatal("tail must keep the END of stderr (the stack trace)")
	}
	b, err := os.ReadFile(LogPath())
	if err != nil {
		t.Fatalf("no modules.log stanza: %v", err)
	}
	if !strings.Contains(string(b), "module=stderrmod") || !strings.Contains(string(b), "TAIL-MARKER") {
		t.Fatalf("stanza missing fields:\n%s", b)
	}
}

func TestInvokeSerializesPerModule(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	run := func(m Module) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				Invoke(context.Background(), m, helperCmd("sleep-json", "400"), NewEnvelope(EventAction, m, nil), CapReply, 30*time.Second)
			}()
		}
		wg.Wait()
		return time.Since(start)
	}
	same := testModule(t, "serial")
	if d := run(same); d < 750*time.Millisecond {
		t.Fatalf("same-module overlap not serialized: %s", d)
	}
	// Distinct modules are free to overlap — assert they at least both ran
	// (no global lock): two 400ms sleeps well under the serialized floor.
	a, b := testModule(t, "par-a"), testModule(t, "par-b")
	start := time.Now()
	var wg sync.WaitGroup
	for _, m := range []Module{a, b} {
		wg.Add(1)
		go func(m Module) {
			defer wg.Done()
			Invoke(context.Background(), m, helperCmd("sleep-json", "400"), NewEnvelope(EventAction, m, nil), CapReply, 30*time.Second)
		}(m)
	}
	wg.Wait()
	if d := time.Since(start); d >= 750*time.Millisecond {
		t.Logf("note: distinct modules took %s (loaded machine?)", d)
	}
}

func TestResolveArgv(t *testing.T) {
	m := Module{Dir: t.TempDir()}
	if bin, _, err := resolveArgv(m, []string{"pwsh", "-File", "x.ps1"}); err != nil || bin != "pwsh" {
		t.Fatalf("bare name: %q %v", bin, err)
	}
	if bin, args, err := resolveArgv(m, []string{"bin/tool", "--flag"}); err != nil || !strings.HasPrefix(bin, m.Dir) || args[0] != "--flag" {
		t.Fatalf("relative: %q %v %v", bin, args, err)
	}
	if _, _, err := resolveArgv(m, []string{"../escape"}); err == nil {
		t.Fatal("escape must be rejected")
	}
	if _, _, err := resolveArgv(m, nil); err == nil {
		t.Fatal("empty argv must be rejected")
	}
}
