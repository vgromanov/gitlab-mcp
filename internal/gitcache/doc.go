// Package gitcache is the opt-in native Go bare object cache for gitlab-mcp.
//
// It is disabled by default. When enabled on Linux or Darwin it owns a private
// descriptor-relative cache root with exclusive lifetime locking, immutable
// pack/index/manifest generations, reservation-before-write accounting, and
// in-process reader pins. Windows builds compile; enabling the cache there
// fails closed with ErrPlatform.
//
// Acquisition uses native HTTPS (go-git UploadPackSession with an explicit
// http.Client) or native Go SSH (reviewed sshconfig/trust/identity path). It
// does not invoke installed Git, external ssh, credential helpers, hooks,
// checkouts, submodules, file:// transport, or subprocess FS helpers.
//
// Ordinary API metadata, approval, discussion, and raw-file tools do not
// require this package. Exact-diff / rename / hunk / GitLab anchor parity is
// owned by RVG-145 and is not implemented here.
package gitcache
