package auth

// ProtectUserDirectory hardens a freshly created directory before its contents
// are populated. On Windows it installs a protected current-user/SYSTEM DACL
// with inheritance for children; on other platforms it sets mode 0700.
// Explicit migrations use this for staging encrypted credential files.
func ProtectUserDirectory(path string) error { return privatePath(path, true) }
