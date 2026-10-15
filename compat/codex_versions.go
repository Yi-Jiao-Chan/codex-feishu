package compat

// VerifiedCodexVersion reports whether a Codex app-server version has passed
// the bridge's compatibility verification. The protocol schema baseline stays
// pinned separately; this gate is an exact allowlist of observed versions.
func VerifiedCodexVersion(version string) bool {
	switch version {
	case "0.162.0", "0.162.1":
		return true
	default:
		return false
	}
}

// VerifiedCodexVersions returns a fresh compatibility list for each call.
func VerifiedCodexVersions() []string {
	return []string{"0.162.0", "0.162.1"}
}

// Creation's project and persistent-source fields were verified separately
// against 0.162.1. Phase1 compatibility does not imply creation compatibility.
func VerifiedCreationCodexVersion(version string) bool { return version == "0.162.1" }
