package cgroup

import (
	"testing"
)

func TestNormalizeCgroupPath(t *testing.T) {
	rel := "ggate_test"
	expectedRel := "/sys/fs/cgroup/ggate_test"
	if got := NormalizeCgroupPath(rel); got != expectedRel {
		t.Fatalf("NormalizeCgroupPath(%q) = %q, want %q", rel, got, expectedRel)
	}

	abs := "/sys/fs/cgroup/foo"
	expectedAbs := "/sys/fs/cgroup/foo"
	if got := NormalizeCgroupPath(abs); got != expectedAbs {
		t.Fatalf("NormalizeCgroupPath(%q) = %q, want %q", abs, got, expectedAbs)
	}
}

func TestResolveInode(t *testing.T) {
	dir := t.TempDir()
	inode, err := ResolveInode(dir)
	if err != nil {
		t.Fatalf("ResolveInode(%q) failed: %v", dir, err)
	}
	if inode == 0 {
		t.Fatalf("ResolveInode(%q) returned inode 0, expected > 0", dir)
	}
}
