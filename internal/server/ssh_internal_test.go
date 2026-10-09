package server

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// flakyListener fails Accept with a transient error a few times, then reports itself closed.
type flakyListener struct {
	net.Listener
	failures int
	accepts  int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.accepts++
	if l.accepts <= l.failures {
		return nil, errors.New("accept: too many open files")
	}
	return nil, net.ErrClosed
}

func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	listener := &flakyListener{failures: 3}
	done := make(chan struct{})
	go func() {
		(&SSHServer{}).acceptLoop(listener)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not exit after the listener was closed")
	}
	if listener.accepts != listener.failures+1 {
		t.Errorf("Accept called %d times, want %d (loop must retry transient errors)", listener.accepts, listener.failures+1)
	}
}

func TestHostKeyIsNotReplacedWhenUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
	path := filepath.Join(t.TempDir(), "host_key.pem")
	if _, err := getOrCreateHostKey(path); err != nil {
		t.Fatalf("failed to create host key: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write-only: unreadable, but the old code could still truncate and replace it
	if err := os.Chmod(path, 0200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })

	if _, err := getOrCreateHostKey(path); err == nil {
		t.Error("expected an error for an unreadable host key")
	}
	_ = os.Chmod(path, 0600)
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Error("unreadable host key was overwritten with a new one")
	}
}

func TestHostKeyGenerationUsesEd25519(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key.pem")
	signer, err := getOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("failed to create host key: %v", err)
	}

	pubKey := signer.PublicKey()
	if pubKey.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("host key type = %q, want %q", pubKey.Type(), ssh.KeyAlgoED25519)
	}

	// Verify that the generated key file can be reloaded and retains the same public key
	reloadedSigner, err := getOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("failed to reload generated host key: %v", err)
	}
	if string(reloadedSigner.PublicKey().Marshal()) != string(pubKey.Marshal()) {
		t.Errorf("reloaded public key does not match generated public key")
	}
}
