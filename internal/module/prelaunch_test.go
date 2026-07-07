package module

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestManifestPreLaunchValidation(t *testing.T) {
	base := `{"api":1,"name":"m","preLaunch":{"command":%s}}`
	cases := []struct {
		name    string
		command string
		wantErr string
	}{
		{"bare name ok", `["pwsh","-NoProfile","-File","hook.ps1"]`, ""},
		{"relative ok", `["bin/hook","--check"]`, ""},
		{"empty rejected", `[]`, "at least one element"},
		{"absolute rejected", `["C:\\evil\\hook.exe"]`, "inside the module directory"},
		{"escape rejected", `["../escape"]`, "inside the module directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(strings.Replace(base, "%s", c.command, 1)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
	// Backward compat: a manifest without preLaunch keeps a nil hook.
	m, err := ParseManifest([]byte(`{"api":1,"name":"m"}`))
	if err != nil || m.PreLaunch != nil {
		t.Fatalf("no-hook manifest: %+v %v", m.PreLaunch, err)
	}
}

func TestPreLaunchMods(t *testing.T) {
	mk := func(name string, hook bool) Module {
		m := Module{Entry: Entry{Name: name}, Manifest: Manifest{API: 1, Name: name}}
		if hook {
			m.Manifest.PreLaunch = &Handler{Command: []string{"x"}}
		}
		return m
	}
	got := PreLaunchMods([]Module{mk("a", true), mk("b", false), mk("c", true)})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("filter/order: %+v", got)
	}
	if PreLaunchMods(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

func TestExecPreLaunchEnvelopeFile(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())
	m := testModule(t, "hookmod")
	m.Manifest.PreLaunch = &Handler{Command: []string{"whatever"}}
	row := MissionRow{Key: "p/m1", Title: "T", Cwd: "C:/work"}
	env := NewEnvelope(EventPreLaunch, m, PreLaunchPayload{Source: "resume", Cwd: "C:/work", Mission: &row})
	cmd, cleanup, err := ExecPreLaunch(m, env)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if cmd.SysProcAttr != nil || cmd.Stdin != nil || cmd.Stdout != nil {
		t.Fatal("preLaunch must be a plain interactive cmd (real terminal, no flags)")
	}
	var envFile string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "HOUSTON_EVENT_FILE="); ok {
			envFile = v
		}
	}
	if envFile == "" {
		t.Fatal("HOUSTON_EVENT_FILE not set")
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Event   string `json:"event"`
		Payload struct {
			Source  string      `json:"source"`
			Cwd     string      `json:"cwd"`
			Mission *MissionRow `json:"mission"`
			Account *AccountRow `json:"account"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Event != EventPreLaunch || back.Payload.Source != "resume" ||
		back.Payload.Cwd != "C:/work" || back.Payload.Mission == nil || back.Payload.Account != nil {
		t.Fatalf("envelope payload: %+v", back)
	}
	cleanup()
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the envelope")
	}
}
