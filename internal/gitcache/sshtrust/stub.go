//go:build !linux && !darwin

package sshtrust

import (
	"context"
	"errors"
	"net"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
)

var (
	ErrTrust             = errors.New("ssh host-key policy rejected the server")
	ErrTrustFiles        = errors.New("ssh configured known_hosts files are unavailable, changed, or unsupported")
	ErrUpdate            = errors.New("ssh authenticated host-key update failed")
	ErrUpdateUnsupported = errors.New("ssh host-key update requires unsupported known_hosts patterns or file layout")
	ErrConfirmation      = errors.New("ssh host-key policy requires interactive confirmation; native transport is noninteractive")
)

type Manager struct{}
type Transition struct{}

func (m *Manager) Finalize(error) error      { return ErrTrustFiles }
func (m *Manager) Transitions() []Transition { return nil }
func (m *Manager) ValidateRefresh(context.Context, *Manager, []Transition) error {
	return ErrTrustFiles
}

type Updates struct{ done chan struct{} }

func New(context.Context, sshconfig.Config, string) (*Manager, error) { return nil, ErrTrustFiles }
func (m *Manager) Callback(string, net.Addr, ssh.PublicKey) error     { return ErrTrust }
func (m *Manager) Start(context.Context, ssh.Conn, <-chan *ssh.Request) *Updates {
	u := &Updates{done: make(chan struct{})}
	close(u.done)
	return u
}
func (m *Manager) Verified() bool       { return false }
func (m *Manager) UpdateStatus() string { return "unsupported" }
func (u *Updates) Wait()                { <-u.done }
func (u *Updates) Sync() error          { return ErrUpdateUnsupported }
func (u *Updates) Error() error         { return ErrUpdateUnsupported }

func (m *Manager) Provenance() string { return "unsupported" }

// SetReplacePostRenamePhaseForTest is a no-op on unsupported platforms.
func SetReplacePostRenamePhaseForTest(string, func(string)) func() { return func() {} }
