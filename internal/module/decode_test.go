package module

import (
	"strings"
	"testing"
)

func TestDecodeReply(t *testing.T) {
	type reply struct {
		Status string `json:"status"`
		N      int    `json:"n"`
	}
	cases := []struct {
		name    string
		raw     string
		wantErr string // substring, "" = success
		want    reply
	}{
		{name: "plain", raw: `{"status":"ok","n":2}`, want: reply{"ok", 2}},
		{name: "utf8 bom stripped", raw: "\xef\xbb\xbf" + `{"status":"bom"}`, want: reply{Status: "bom"}},
		{name: "utf16 le rejected", raw: "\xff\xfe{\x00}\x00", wantErr: "UTF-16"},
		{name: "utf16 be rejected", raw: "\xfe\xff\x00{\x00}", wantErr: "UTF-16"},
		{name: "leading crlf and spaces", raw: " \r\n\t" + `{"status":"ws"}`, want: reply{Status: "ws"}},
		{name: "empty is a no-op", raw: "", want: reply{}},
		{name: "whitespace only is a no-op", raw: " \r\n \t ", want: reply{}},
		{name: "trailing noise tolerated", raw: `{"status":"ok"}` + "\r\nWrite-Host was here", want: reply{Status: "ok"}},
		{name: "leading garbage rejected", raw: "Loading...\n" + `{"status":"late"}`, wantErr: "invalid reply"},
		{name: "wrong-typed known field", raw: `{"status":"ok","n":"three"}`, wantErr: "invalid reply"},
		{name: "unknown fields ignored", raw: `{"status":"ok","future":{"x":1}}`, want: reply{Status: "ok"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got reply
			err := DecodeReply([]byte(c.raw), &got)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want error containing %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDecodeReplyErrorSnippetIsBounded(t *testing.T) {
	var v struct{}
	err := DecodeReply([]byte(strings.Repeat("garbage ", 200)), &v)
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error message unbounded: %d bytes", len(err.Error()))
	}
}
