package engine

import "testing"

func TestRewriteGitURL(t *testing.T) {
	cases := []struct {
		url, token, want string
	}{
		{"https://github.com/u/r.git", "", "https://github.com/u/r.git"},
		{"https://github.com/u/r.git", "tok1", "https://x-access-token:tok1@github.com/u/r.git"},
		{"https://gitlab.example.com/u/r.git", "tok2", "https://oauth2:tok2@gitlab.example.com/u/r.git"},
		{"https://gitee.com/u/r.git", "tok3", "https://git:tok3@gitee.com/u/r.git"},
		{"git@github.com:u/r.git", "tok4", "git@github.com:u/r.git"}, // ssh 不改写
	}
	for _, c := range cases {
		if got := rewriteGitURL(c.url, c.token); got != c.want {
			t.Errorf("rewriteGitURL(%q, %q) = %q, want %q", c.url, c.token, got, c.want)
		}
	}
}

func TestRedactGitURL(t *testing.T) {
	in := "https://oauth2:secret@gitlab.example.com/u/r.git"
	got := redactGitURL(in)
	if got != "https://***@gitlab.example.com/u/r.git" {
		t.Fatalf("token 未脱敏: %q", got)
	}
	if redactGitURL("https://github.com/u/r.git") != "https://github.com/u/r.git" {
		t.Fatal("无 token URL 不应变")
	}
}

func TestShellQuoteSingle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"echo hi", "'echo hi'"},
		{"echo it's", `'echo it'\''s'`},
		{"a\nb 'c'", `'a
b '\''c'\'''`},
	}
	for _, c := range cases {
		if got := shellQuoteSingle(c.in); got != c.want {
			t.Errorf("shellQuoteSingle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
