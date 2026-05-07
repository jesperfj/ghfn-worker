// Package gitsync clones and refreshes the script repository by shelling out
// to git. Keeping a process boundary avoids pulling in go-git and lets users
// customize git config (askpass, http.proxy, etc.) the usual way.
package gitsync

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options configures a sync.
type Options struct {
	// RepoURL is the upstream URL. https://github.com/owner/repo.git is the
	// expected form; if Token is set, it is injected as basic-auth user
	// "x-access-token".
	RepoURL string

	// Branch is the branch to track. Required.
	Branch string

	// Dir is the local checkout directory. Created on first clone.
	Dir string

	// Token is a GitHub PAT (or installation token). Optional; when empty
	// RepoURL is used as-is, suitable for public repos or environments with
	// ambient credentials (SSH agent, gh-cli, GIT_ASKPASS, etc.).
	Token string
}

// Result describes the post-sync state.
type Result struct {
	CommitSHA string
	Cloned    bool // true if this call performed the initial clone
}

// EnsureRepo brings Dir to the tip of origin/Branch. On first call it clones;
// on subsequent calls it fetches and resets --hard. Resetting is intentional —
// the local checkout is a cache of upstream, never a working tree.
func EnsureRepo(ctx context.Context, opts Options) (*Result, error) {
	if opts.RepoURL == "" {
		return nil, fmt.Errorf("RepoURL is required")
	}
	if opts.Branch == "" {
		return nil, fmt.Errorf("Branch is required")
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("Dir is required")
	}

	authedURL, err := injectToken(opts.RepoURL, opts.Token)
	if err != nil {
		return nil, err
	}

	gitDir := filepath.Join(opts.Dir, ".git")
	cloned := false
	if _, err := os.Stat(gitDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat %s: %w", gitDir, err)
		}
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			return nil, err
		}
		if err := run(ctx, "", "git", "clone", "--branch", opts.Branch, "--single-branch", authedURL, opts.Dir); err != nil {
			return nil, err
		}
		cloned = true
	} else {
		// Make sure the remote URL is current (token may have been rotated).
		if err := run(ctx, opts.Dir, "git", "remote", "set-url", "origin", authedURL); err != nil {
			return nil, err
		}
		if err := run(ctx, opts.Dir, "git", "fetch", "--depth=1", "origin", opts.Branch); err != nil {
			return nil, err
		}
		if err := run(ctx, opts.Dir, "git", "reset", "--hard", "FETCH_HEAD"); err != nil {
			return nil, err
		}
		// Drop anything else that might have leaked in.
		if err := run(ctx, opts.Dir, "git", "clean", "-fdx"); err != nil {
			return nil, err
		}
	}

	sha, err := capture(ctx, opts.Dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &Result{CommitSHA: strings.TrimSpace(sha), Cloned: cloned}, nil
}

func injectToken(rawURL, token string) (string, error) {
	if token == "" {
		return rawURL, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse repo URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return rawURL, nil // SSH or git:// — leave alone, token doesn't apply
	}
	u.User = url.UserPassword("x-access-token", token)
	return u.String(), nil
}

// maskURL hides the token in any URL we may end up logging.
func maskURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		// Mask any token that snuck into the args.
		safe := make([]string, len(args))
		for i, a := range args {
			safe[i] = maskURL(a)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(safe, " "), err)
	}
	return nil
}

func capture(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
