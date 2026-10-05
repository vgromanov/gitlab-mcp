//go:build unix && !linux && !darwin && !ios

package intentstore

// rejectAccessACL is a no-op on Unix flavors without a portable ACL API
// in this build. Linux and Darwin inspect access ACLs.
func rejectAccessACL(string) error { return nil }
