package compat

// VerifiedCodexVersion reports whether a Codex app-server version has passed
// the bridge's historical compatibility verification. Legacy artifacts and
// opt-in migration use this set. The service connects independently of it.
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
