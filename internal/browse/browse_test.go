package browse

import "testing"

func TestCleanURL(t *testing.T) {
	cases := map[string]string{
		`https://claude.ai/oauth/authorize?x=1`:     `https://claude.ai/oauth/authorize?x=1`,
		`"https://claude.ai/oauth/authorize?x=1"`:   `https://claude.ai/oauth/authorize?x=1`,
		`  "https://example.com"  `:                 `https://example.com`,
		`"`:                                         `"`, // lone quote: left alone
	}
	for in, want := range cases {
		if got := CleanURL(in); got != want {
			t.Errorf("CleanURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsHTTP(t *testing.T) {
	yes := []string{"https://claude.ai/x", "http://localhost:8080/cb"}
	no := []string{`C:\Users\x`, "--paths", "file:///etc/passwd", "vscode://x", ""}
	for _, s := range yes {
		if !IsHTTP(s) {
			t.Errorf("IsHTTP(%q) should be true", s)
		}
	}
	for _, s := range no {
		if IsHTTP(s) {
			t.Errorf("IsHTTP(%q) should be false", s)
		}
	}
}

func TestIsLogin(t *testing.T) {
	yes := []string{
		"https://claude.ai/oauth/authorize?code=true&client_id=x",
		"https://console.anthropic.com/oauth/authorize?x=1",
	}
	no := []string{
		"https://claude.ai/new",                       // ordinary product link
		"https://docs.claude.com/oauth/authorize",     // wrong host
		"https://claude.ai.evil.com/oauth/authorize",  // suffix spoof
		"https://evil.com/?u=claude.ai/oauth",         // host in query
		"https://github.com/anthropics/claude-code",   // regular link
	}
	for _, s := range yes {
		if !IsLogin(s) {
			t.Errorf("IsLogin(%q) should be true", s)
		}
	}
	for _, s := range no {
		if IsLogin(s) {
			t.Errorf("IsLogin(%q) should be false", s)
		}
	}
}

func TestPrivateFlag(t *testing.T) {
	cases := map[string]string{
		"ChromeHTML":                     "--incognito",
		"BraveHTML":                      "--incognito",
		"MSEdgeHTM":                      "--inprivate",
		"FirefoxURL-308046B0AF4A39CB":    "--private-window",
		"firefox.desktop":                "--private-window",
		"microsoft-edge.desktop":         "--inprivate",
		"OperaStable":                    "", // unknown family → caller falls back
		"":                               "",
	}
	for in, want := range cases {
		if got := privateFlag(in); got != want {
			t.Errorf("privateFlag(%q) = %q, want %q", in, got, want)
		}
	}
}
