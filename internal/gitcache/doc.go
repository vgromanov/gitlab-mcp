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
// require this package. When a service is enabled, get_merge_request acquires
// that merge request through GitCacheAuthorizer and Service.Acquire.
// get_merge_request_diff_window (RVG-145) may compare those authorized
// objects with a locked-down no-checkout git subprocess; this package still
// does not invoke installed Git for acquisition.
package gitcache
