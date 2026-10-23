package compat

// Managed worktree publication is a desktop filesystem compatibility layer,
// not an App Server RPC. The inspected desktop scanner accepts named buckets
// only after .codex-worktree-name appears. Revalidate it on desktop upgrades.
const ManagedWorktreeDesktopVersion = "26.1016.62342"

func VerifiedWorktreeCodexVersion(version string) bool { return version == "0.162.1" }
