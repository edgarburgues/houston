package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"houston/internal/accounts"
	"houston/internal/flock"
)

// healEnv builds a temp shared store + one account dir with every ShareDir
// correctly linked, returning the account. Tests then break specific links.
func healEnv(t *testing.T) accounts.Account {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOUSTON_SHARED_DIR", filepath.Join(root, "shared"))
	t.Setenv("HOUSTON_ACCOUNTS_DIR", filepath.Join(root, "accounts"))
	a := accounts.Account{ID: "t1"}
	if err := EnsureShared(); err != nil {
		t.Fatal(err)
	}
	cd := a.ResolveConfigDir()
	if err := os.MkdirAll(cd, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range ShareDirs {
		if err := makeLink(filepath.Join(cd, d), filepath.Join(SharedDir(), d)); err != nil {
			t.Skipf("cannot create links here: %v", err)
		}
	}
	return a
}

func TestHealHappyPathIsQuietAndChangesNothing(t *testing.T) {
	a := healEnv(t)
	res := Heal([]accounts.Account{a})
	if !res.Quiet() || len(res.Relinked) != 0 {
		t.Fatalf("healthy layout must be a no-op: %+v", res)
	}
}

func TestHealSkipsMissingAccountDir(t *testing.T) {
	a := healEnv(t)
	ghost := accounts.Account{ID: "ghost"} // no config dir on disk
	res := Heal([]accounts.Account{a, ghost})
	if !res.Quiet() || len(res.Relinked) != 0 {
		t.Fatalf("missing account dirs are doctor's business: %+v", res)
	}
}

func TestHealRelinksMissing(t *testing.T) {
	a := healEnv(t)
	link := filepath.Join(a.ResolveConfigDir(), "plans")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	res := Heal([]accounts.Account{a})
	if !res.Quiet() {
		t.Fatalf("relink must be silent: %+v", res)
	}
	if len(res.Relinked) != 1 || !strings.Contains(res.Relinked[0], "t1/plans") {
		t.Fatalf("relinked: %v", res.Relinked)
	}
	// The repaired link routes writes into the shared store.
	if err := os.WriteFile(filepath.Join(link, "p.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(SharedDir(), "plans", "p.md")); err != nil {
		t.Fatalf("write through healed link not in shared: %v", err)
	}
}

func TestHealReplacesEmptyRealDir(t *testing.T) {
	a := healEnv(t)
	link := filepath.Join(a.ResolveConfigDir(), "todos")
	os.Remove(link)
	os.MkdirAll(link, 0o755)
	res := Heal([]accounts.Account{a})
	if !res.Quiet() || len(res.Relinked) != 1 {
		t.Fatalf("%+v", res)
	}
	if classify(link, filepath.Join(SharedDir(), "todos")) != LinkOK {
		t.Fatal("todos not relinked")
	}
}

func TestHealFixesWrongTarget(t *testing.T) {
	a := healEnv(t)
	link := filepath.Join(a.ResolveConfigDir(), "plans")
	os.Remove(link)
	other := filepath.Join(t.TempDir(), "elsewhere")
	os.MkdirAll(other, 0o755)
	if err := makeLink(link, other); err != nil {
		t.Skipf("cannot create links here: %v", err)
	}
	res := Heal([]accounts.Account{a})
	if !res.Quiet() || len(res.Relinked) != 1 {
		t.Fatalf("%+v", res)
	}
	if classify(link, filepath.Join(SharedDir(), "plans")) != LinkOK {
		t.Fatal("plans still points elsewhere")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("re-linking must never remove the old target dir")
	}
}

// driftWithData replaces the plans link with a real dir holding files — the
// exact recurring bug: Claude Code recreated the dir while the link was gone.
func driftWithData(t *testing.T, a accounts.Account, files map[string]string) string {
	t.Helper()
	link := filepath.Join(a.ResolveConfigDir(), "plans")
	os.Remove(link)
	for name, content := range files {
		p := filepath.Join(link, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return link
}

func TestHealMergesDriftedData(t *testing.T) {
	a := healEnv(t)
	link := driftWithData(t, a, map[string]string{
		"curried-zooming-popcorn.md":  "the plan",
		filepath.Join("sub", "n.md"): "nested",
	})
	res := Heal([]accounts.Account{a})
	if len(res.Skipped) != 0 || len(res.Merged) != 1 {
		t.Fatalf("%+v", res)
	}
	shared := filepath.Join(SharedDir(), "plans")
	for _, rel := range []string{"curried-zooming-popcorn.md", filepath.Join("sub", "n.md")} {
		if _, err := os.Stat(filepath.Join(shared, rel)); err != nil {
			t.Fatalf("%s not merged into shared: %v", rel, err)
		}
	}
	if classify(link, shared) != LinkOK {
		t.Fatal("plans not relinked after merge")
	}
	// The plan is reachable again from the account path.
	if b, err := os.ReadFile(filepath.Join(link, "curried-zooming-popcorn.md")); err != nil || string(b) != "the plan" {
		t.Fatalf("merged content unreachable through the link: %v", err)
	}
}

func TestHealMergeDeduplicatesIdenticalContent(t *testing.T) {
	a := healEnv(t)
	shared := filepath.Join(SharedDir(), "plans")
	os.WriteFile(filepath.Join(shared, "same.md"), []byte("identical"), 0o644)
	driftWithData(t, a, map[string]string{"same.md": "identical"})
	res := Heal([]accounts.Account{a})
	if len(res.Merged) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("%+v", res)
	}
	ents, _ := os.ReadDir(shared)
	if len(ents) != 1 {
		t.Fatalf("dedup must not create a conflict copy: %v", ents)
	}
}

func TestHealMergeKeepsBothOnConflict(t *testing.T) {
	a := healEnv(t)
	shared := filepath.Join(SharedDir(), "plans")
	os.WriteFile(filepath.Join(shared, "plan.md"), []byte("shared version"), 0o644)
	driftWithData(t, a, map[string]string{"plan.md": "account version"})
	res := Heal([]accounts.Account{a})
	if len(res.Merged) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(shared, "plan.md")); string(b) != "shared version" {
		t.Fatal("shared copy must never be overwritten")
	}
	if b, err := os.ReadFile(filepath.Join(shared, "plan.from-t1.md")); err != nil || string(b) != "account version" {
		t.Fatalf("conflict copy missing: %v", err)
	}
}

func TestHealSkipsFileInTheWay(t *testing.T) {
	a := healEnv(t)
	link := filepath.Join(a.ResolveConfigDir(), "plans")
	os.Remove(link)
	os.WriteFile(link, []byte("not a dir"), 0o644)
	res := Heal([]accounts.Account{a})
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "file") {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(link); string(b) != "not a dir" {
		t.Fatal("the stray file must be left untouched")
	}
}

func TestHealMergeYieldsToConcurrentHeal(t *testing.T) {
	a := healEnv(t)
	driftWithData(t, a, map[string]string{"p.md": "x"})
	lk, err := flock.Acquire(filepath.Join(SharedDir(), ".heal.lock"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	res := Heal([]accounts.Account{a})
	if len(res.Merged) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("merge must yield under the held lock: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(a.ResolveConfigDir(), "plans", "p.md")); err != nil {
		t.Fatal("yielding must leave the data in place")
	}
}

func TestHealIsIdempotent(t *testing.T) {
	a := healEnv(t)
	driftWithData(t, a, map[string]string{"p.md": "x"})
	if res := Heal([]accounts.Account{a}); len(res.Merged) != 1 {
		t.Fatalf("first pass: %+v", res)
	}
	if res := Heal([]accounts.Account{a}); !res.Quiet() || len(res.Relinked) != 0 {
		t.Fatalf("second pass must be a no-op: %+v", res)
	}
}

func TestUniqueDest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plan.md")
	if got := uniqueDest(p, "w2"); got != p {
		t.Fatalf("free path must pass through: %q", got)
	}
	os.WriteFile(p, []byte("a"), 0o644)
	first := filepath.Join(dir, "plan.from-w2.md")
	if got := uniqueDest(p, "w2"); got != first {
		t.Fatalf("got %q, want %q", got, first)
	}
	os.WriteFile(first, []byte("b"), 0o644)
	second := filepath.Join(dir, "plan.from-w2-2.md")
	if got := uniqueDest(p, "w2"); got != second {
		t.Fatalf("got %q, want %q", got, second)
	}
}
