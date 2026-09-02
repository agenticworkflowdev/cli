//go:build windows

package gitrepo

import "testing"

func TestSamePathHandlesWindowsCaseAndSeparatorVariants(t *testing.T) {
	if !samePath(`C:\Repo\Worktree`, `c:/repo/worktree`) {
		t.Fatal("equivalent Windows worktree paths did not match")
	}
}
