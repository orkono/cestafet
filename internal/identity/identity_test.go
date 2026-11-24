package identity

import (
	"testing"
	"time"
)

func TestNormalizeRemote(t *testing.T) {
	resolve := func(alias string) string {
		if alias == "github-work" {
			return "github.com"
		}
		return ""
	}
	cases := map[string]string{
		"git@github.com:orkono/cestafet.git":                           "github.com/orkono/cestafet",
		"https://github.com/orkono/cestafet":                           "github.com/orkono/cestafet",
		"https://user@GitHub.com/orkono/cestafet.git":                  "github.com/orkono/cestafet",
		"https://user:ghp_secret123@github.com/orkono/cestafet.git":    "github.com/orkono/cestafet",
		"https://oauth2:glpat-secret@gitlab.example.com:8443/team/app": "gitlab.example.com/team/app",
		"https://github.com/12345/myrepo.git":                          "github.com/12345/myrepo",
		"git@gitlab.com:42/myrepo.git":                                 "gitlab.com/42/myrepo",
		"ssh://git@gitlab.example.com:22/42/app.git":                   "gitlab.example.com/42/app",
		"ssh://git@gitlab.example.com:22/team/app":                     "gitlab.example.com/team/app",
		"ssh://git@github-work:22/orkono/cestafet.git":                 "github.com/orkono/cestafet",
		"HTTPS://GitHub.com/orkono/cestafet.git//":                     "github.com/orkono/cestafet",
		"git@github-work:orkono/cestafet.git":                          "github.com/orkono/cestafet",
		"git@unknown-alias:team/app.git":                               "unknown-alias/team/app",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in, resolve); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProfileName(t *testing.T) {
	cases := map[string]string{
		"":                         "claude",
		"/Users/x/.claude-orkono":  "claude-orkono",
		"/Users/x/.claude-orkono/": "claude-orkono",
		"/opt/profiles/work":       "work",
	}
	for in, want := range cases {
		if got := ProfileName(in); got != want {
			t.Errorf("ProfileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeAncestor(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	tree := map[int]Proc{
		// macOS: the hook's parent is claude.
		40: {PID: 40, PPID: 39, Name: "/bin/bash"},
		39: {PID: 39, PPID: 38, Name: "/Users/x/.local/bin/claude"},
		38: {PID: 38, PPID: 1, Name: "zsh"},
		// Windows, as measured: cestafet.exe < bash.exe < bash.exe < claude.exe.
		70: {PID: 70, PPID: 71, Name: "cestafet.exe", Created: t0.Add(3 * time.Second)},
		71: {PID: 71, PPID: 72, Name: "bash.exe", Created: t0.Add(2 * time.Second)},
		72: {PID: 72, PPID: 73, Name: "bash.exe", Created: t0.Add(1 * time.Second)},
		73: {PID: 73, PPID: 4, Name: "claude.exe", Created: t0},
		// A reused pid: 81's recorded parent 82 is now a claude.exe started after 81.
		81: {PID: 81, PPID: 82, Name: "bash.exe", Created: t0},
		82: {PID: 82, PPID: 4, Name: "claude.exe", Created: t0.Add(time.Minute)},
	}
	info := func(pid int) (Proc, bool) {
		p, ok := tree[pid]
		return p, ok
	}
	if got := claudeAncestor(40, info); got.PID != 39 {
		t.Fatalf("macOS ancestor = %+v, want 39", got)
	}
	if got := claudeAncestor(38, info); got.PID != 0 {
		t.Fatalf("ancestor without claude = %+v, want none", got)
	}
	if got := claudeAncestor(71, info); got.PID != 73 || !got.Created.Equal(t0) {
		t.Fatalf("Windows ancestor = %+v, want 73 created %v", got, t0)
	}
	if got := claudeAncestor(81, info); got.PID != 0 {
		t.Fatalf("reused pid accepted: %+v", got)
	}
}
