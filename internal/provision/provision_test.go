package provision

import (
	"os"
	"path/filepath"
	"testing"

	"houston/internal/accounts"
)

func TestClassifyNonLinkStates(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "shared")
	os.MkdirAll(target, 0o755)

	// missing
	if s := classify(filepath.Join(root, "nope"), target); s != LinkMissing {
		t.Errorf("missing: got %v", s)
	}
	// real empty dir
	empty := filepath.Join(root, "empty")
	os.MkdirAll(empty, 0o755)
	if s := classify(empty, target); s != LinkRealEmpty {
		t.Errorf("empty: got %v", s)
	}
	// real dir with data
	full := filepath.Join(root, "full")
	os.MkdirAll(full, 0o755)
	os.WriteFile(filepath.Join(full, "x"), []byte("y"), 0o644)
	if s := classify(full, target); s != LinkRealData {
		t.Errorf("data: got %v", s)
	}
	// a file in the way
	file := filepath.Join(root, "file")
	os.WriteFile(file, []byte("z"), 0o644)
	if s := classify(file, target); s != LinkFile {
		t.Errorf("file: got %v", s)
	}
}

func TestClassifyLinkOK(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "shared")
	os.MkdirAll(target, 0o755)
	link := filepath.Join(root, "link")
	if err := makeLink(link, target); err != nil {
		t.Skipf("could not create link (privileges?): %v", err)
	}
	if s := classify(link, target); s != LinkOK {
		t.Errorf("correct link: got %v", s)
	}
	other := filepath.Join(root, "other")
	os.MkdirAll(other, 0o755)
	if s := classify(link, other); s != LinkWrong {
		t.Errorf("link to a different target should be Wrong: got %v", s)
	}
}

func TestSeedConfigStripsIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)               // posix
	t.Setenv("USERPROFILE", home)        // windows
	os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"hasCompletedOnboarding":true,"oauthAccount":{"emailAddress":"x@y.com"}}`), 0o600)
	out := string(seedConfigJSON())
	if want := "hasCompletedOnboarding"; !contains(out, want) {
		t.Errorf("seed should keep %q: %s", want, out)
	}
	if contains(out, "oauthAccount") {
		t.Errorf("seed must NOT include oauthAccount: %s", out)
	}
}

func TestFixIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOUSTON_SHARED_DIR", filepath.Join(home, "shared"))
	t.Setenv("HOUSTON_ACCOUNTS_DIR", filepath.Join(home, "accounts"))

	accs := []accounts.Account{{ID: "work"}}
	if _, err := Fix(accs); err != nil {
		t.Skipf("Fix failed (possibly missing link privileges): %v", err)
	}
	// second pass: no error, and everything reports OK
	if _, err := Fix(accs); err != nil {
		t.Fatalf("second Fix pass failed: %v", err)
	}
	_, reports := Audit(accs)
	for _, d := range reports[0].Dirs {
		if !d.State.OK() {
			t.Errorf("after Fix, %s should be linked, is %v", d.Name, d.State)
		}
	}
}

func TestResyncSettingsOverwrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	shared := filepath.Join(home, "shared")
	t.Setenv("HOUSTON_SHARED_DIR", shared)
	t.Setenv("HOUSTON_ACCOUNTS_DIR", filepath.Join(home, "accounts"))

	accs := []accounts.Account{{ID: "work"}}
	cd := accs[0].ResolveConfigDir()
	if err := os.MkdirAll(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(shared, "settings.json"), []byte(`{"v":"new"}`), 0o644)
	os.WriteFile(filepath.Join(cd, "settings.json"), []byte(`{"v":"old"}`), 0o644)

	res, err := ResyncSettings(accs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("expected 1 copied file, got %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(cd, "settings.json"))
	if string(b) != `{"v":"new"}` {
		t.Errorf("per-account settings should be overwritten with the shared copy, got %s", b)
	}
	// the diverged original survives as .bak — a resync is never silent data loss
	bak, err := os.ReadFile(filepath.Join(cd, "settings.json.bak"))
	if err != nil || string(bak) != `{"v":"old"}` {
		t.Errorf("overwritten settings should be kept as .bak, got %q err=%v", bak, err)
	}

	// second pass: identical content → no copy, no new .bak
	res2, err := ResyncSettings(accs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Created) != 0 {
		t.Errorf("identical files should be skipped, got %+v", res2.Created)
	}
}

func TestResyncSettingsSkipsMissingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOUSTON_SHARED_DIR", filepath.Join(home, "shared"))
	t.Setenv("HOUSTON_ACCOUNTS_DIR", filepath.Join(home, "accounts"))

	res, err := ResyncSettings([]accounts.Account{{ID: "ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("account without config dir should be skipped, got %+v", res)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
