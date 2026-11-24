// Package identity derives who a session is: its profile, its repository and
// the claude process that owns it.
package identity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultProfile is the profile name used when CLAUDE_CONFIG_DIR is unset.
const DefaultProfile = "claude"

// Profile returns the profile name: the base name of CLAUDE_CONFIG_DIR without
// a leading dot (".claude-orkono" -> "claude-orkono"), or "claude".
func Profile() string {
	return ProfileName(os.Getenv("CLAUDE_CONFIG_DIR"))
}

// ProfileName turns a config directory into a profile name.
func ProfileName(configDir string) string {
	dir := strings.TrimRight(configDir, "/")
	if dir == "" {
		return DefaultProfile
	}
	return strings.TrimPrefix(filepath.Base(dir), ".")
}

// Repo describes the git repository a directory belongs to.
type Repo struct {
	Root   string // absolute repo root; "" when not a git repo
	Remote string // normalized origin URL, e.g. github.com/orkono/x; "" when none
	Branch string
}

// gitTimeout bounds every git call so a hook never hangs a session.
const gitTimeout = 2 * time.Second

func git(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := Quiet(exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LookupRepo inspects the git repository containing dir.
func LookupRepo(dir string) Repo {
	root := git(dir, "rev-parse", "--show-toplevel")
	if root == "" {
		return Repo{}
	}
	r := Repo{Root: root, Branch: git(root, "rev-parse", "--abbrev-ref", "HEAD")}
	if url := git(root, "remote", "get-url", "origin"); url != "" {
		r.Remote = NormalizeRemote(url, sshHostname)
	}
	return r
}

// NormalizeRemote turns a git remote URL into "host/owner/repo". SSH host
// aliases (e.g. "github-work" from ~/.ssh/config) are resolved through resolve.
func NormalizeRemote(raw string, resolve func(alias string) string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimRight(u, "/")

	var host, path string
	var parsed bool
	if idx := strings.Index(u, "://"); idx >= 0 {
		switch strings.ToLower(u[:idx]) {
		case "https", "http", "ssh", "git", "git+ssh":
			parsed = true
			rest := u[idx+3:]
			authority := rest
			if slash := strings.IndexByte(rest, '/'); slash >= 0 {
				authority, path = rest[:slash], strings.TrimLeft(rest[slash+1:], "/")
			}
			// Strip optional user or user:pass credentials from authority.
			if at := strings.LastIndexByte(authority, '@'); at >= 0 {
				authority = authority[at+1:]
			}
			host = stripPort(authority)
		}
	}
	if !parsed {
		// SCP-like syntax: [user@]host:path (or bare host/path).
		if i := strings.IndexAny(u, ":/"); i >= 0 {
			host, path = u[:i], strings.TrimLeft(u[i+1:], "/")
		} else {
			host = u
		}
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
	}
	if host != "" && !strings.Contains(host, ".") && resolve != nil {
		if real := resolve(host); real != "" {
			host = real
		}
	}
	return strings.ToLower(host) + "/" + path
}

// stripPort removes an explicit :port from a URL authority (keeping IPv6 [::1]).
func stripPort(authority string) string {
	if strings.HasPrefix(authority, "[") {
		if end := strings.IndexByte(authority, ']'); end >= 0 {
			return authority[:end+1]
		}
		return authority
	}
	if colon := strings.IndexByte(authority, ':'); colon >= 0 {
		return authority[:colon]
	}
	return authority
}

// sshHostname resolves an ~/.ssh/config alias to its real host name.
func sshHostname(alias string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := Quiet(exec.CommandContext(ctx, "ssh", "-G", "--", alias)).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.TrimSpace(h)
		}
	}
	return ""
}

// Claude walks up the process tree from the current process and returns the
// Claude Code process that owns it: claude, or claude.exe on Windows, where
// hooks run through Git Bash (cestafet < bash < bash < claude.exe). The zero
// Proc means no Claude Code ancestor was found.
func Claude() Proc {
	return claudeAncestor(os.Getppid(), procInfo)
}

// ClaudePID is Claude().PID.
func ClaudePID() int { return Claude().PID }

func claudeAncestor(start int, info func(pid int) (Proc, bool)) Proc {
	pid := start
	var child Proc
	for depth := 0; pid > 1 && depth < 32; depth++ {
		p, ok := info(pid)
		if !ok {
			return Proc{}
		}
		if depth > 0 && !p.Created.IsZero() && !child.Created.IsZero() && p.Created.After(child.Created) {
			return Proc{} // the recorded parent pid was reused by a younger process
		}
		if IsClaude(p.Name) {
			return p
		}
		child, pid = p, p.PPID
	}
	return Proc{}
}
