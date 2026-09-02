package gitrepo

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorktreePorcelainHandlesMultipleEscapedAndDetachedRecords(t *testing.T) {
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	shaC := strings.Repeat("c", 40)
	contents := "worktree /repo\\worktree\nHEAD " + shaA + "\nbranch refs/heads/main\n\n" +
		"worktree \"/tmp/line\\012break\"\nHEAD " + shaB + "\ndetached\n\n" +
		"worktree C:\\repo\\linked\r\nHEAD " + shaC + "\r\nbranch refs/heads/feature/test\r\n"

	entries, err := parseWorktreePorcelain(contents)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].path != filepath.Clean(`/repo\worktree`) || entries[0].branch != "main" {
		t.Fatalf("platform path entry = %#v", entries[0])
	}
	if entries[1].path != filepath.Clean("/tmp/line\nbreak") || !entries[1].detached {
		t.Fatalf("escaped detached entry = %#v", entries[1])
	}
	if entries[2].path != filepath.Clean(`C:\repo\linked`) || entries[2].branch != "feature/test" {
		t.Fatalf("CRLF entry = %#v", entries[2])
	}
}

func TestParseWorktreePorcelainRejectsMalformedRecords(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, contents := range []string{
		"HEAD " + sha + "\nbranch refs/heads/main\n",
		"worktree /repo\nHEAD short\nbranch refs/heads/main\n",
		"worktree /repo\nHEAD " + sha + "\n",
		"worktree \"unterminated\nHEAD " + sha + "\ndetached\n",
		"worktree /repo\nHEAD " + sha + "\nbranch refs/heads/main\ndetached\n",
		"worktree /repo\nHEAD " + sha + "\ndetached\ndetached\n",
		"worktree /repo\nHEAD " + sha + "\nbare\nbranch refs/heads/main\n",
		"worktree /repo\nHEAD " + sha + "\nbranch refs/heads/main\n\nworktree /repo\nHEAD " + sha + "\nbranch refs/heads/other\n",
		"worktree /one\nHEAD " + sha + "\nbranch refs/heads/main\n\nworktree /two\nHEAD " + sha + "\nbranch refs/heads/main\n",
	} {
		if entries, err := parseWorktreePorcelain(contents); err == nil {
			t.Fatalf("malformed contents produced entries %#v", entries)
		}
	}
}

func TestGitHubRepositoryIdentityAcceptsCommonSafeURLs(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/Owner/Repository.git",
		"https://github.com/Owner/Repository/",
		"ssh://git@github.com/Owner/Repository.git",
		"git@github.com:Owner/Repository.git",
		"git@GITHUB.COM:Owner/Repository.git",
	} {
		identity, err := githubRepositoryIdentity(remote)
		if err != nil || identity != "Owner/Repository" {
			t.Fatalf("identity for %q = %q, %v", remote, identity, err)
		}
	}
}

func TestGitHubRepositoryIdentityRejectsNonGitHubAndAmbiguousURLs(t *testing.T) {
	for _, remote := range []string{
		"file:///owner/repository.git",
		"https://github.com.evil.test/owner/repository.git",
		"https://github.com/owner/repository/extra.git",
		"https://github.com/owner/repository.git?ref=other",
	} {
		if identity, err := githubRepositoryIdentity(remote); err == nil {
			t.Fatalf("unsafe remote %q produced identity %q", remote, identity)
		}
	}
}
