package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"evergit/internal/config"
	"evergit/internal/converter"
	"evergit/internal/resolver"

	"golang.org/x/crypto/ssh"
)

type SSHServer struct {
	cfg       *config.Config
	converter *converter.Manager
	sshConfig *ssh.ServerConfig
}

func NewSSHServer(cfg *config.Config, conv *converter.Manager) (*SSHServer, error) {
	sshConfig := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// Anonymous read-only cloning allows any SSH key.
			log.Printf("[SSH] Connection from %s (%s) authenticating with key %s", conn.RemoteAddr(), conn.User(), key.Type())
			return nil, nil
		},
	}

	hostKeyPath := filepath.Join(cfg.StorageRoot, "host_key.pem")
	signer, err := getOrCreateHostKey(hostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load/generate SSH host key: %w", err)
	}
	sshConfig.AddHostKey(signer)

	return &SSHServer{
		cfg:       cfg,
		converter: conv,
		sshConfig: sshConfig,
	}, nil
}

func (s *SSHServer) Start() (net.Listener, error) {
	listener, err := net.Listen("tcp", s.cfg.SSHAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on SSH address %s: %w", s.cfg.SSHAddr, err)
	}
	s.cfg.SSHAddr = listener.Addr().String()
	log.Printf("Starting Evergit SSH Server on %s", s.cfg.SSHAddr)

	go s.acceptLoop(listener)

	return listener, nil
}

// acceptLoop serves connections until the listener is closed, retrying transient errors
// (e.g. EMFILE) with backoff instead of silently stopping SSH for the process lifetime.
func (s *SSHServer) acceptLoop(listener net.Listener) {
	const initialBackoff = 5 * time.Millisecond
	backoff := initialBackoff
	for {
		nConn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[SSH] Accept failed, retrying in %v: %v", backoff, err)
			time.Sleep(backoff)
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = initialBackoff
		go s.handleConn(nConn)
	}
}

// handshakeTimeout bounds how long an unauthenticated connection may take to complete the handshake.
const handshakeTimeout = 30 * time.Second

func (s *SSHServer) handleConn(nConn net.Conn) {
	_ = nConn.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshConfig)
	if err != nil {
		log.Printf("[SSH] Handshake failed: %v", err)
		return
	}
	// disable deadline
	_ = nConn.SetDeadline(time.Time{})
	defer conn.Close()

	log.Printf("[SSH] New session established from %s", conn.RemoteAddr())

	// discard out-of-band requests (like port forwarding requests or keep-alives)
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		// session: Used for interactive shells, executing single commands, SCP, and SFTP
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Printf("[SSH] Could not accept channel: %v", err)
			continue
		}

		go s.handleSession(ch, reqs)
	}
}

const sshRequestTimeout = 30 * time.Second

func (s *SSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	timer := time.NewTimer(sshRequestTimeout)
	defer timer.Stop()

	session := &sshSession{ctx: ctx, cancel: cancel, ch: ch, reqs: reqs}
	for {
		select {
		case req, ok := <-reqs:
			if !ok {
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(sshRequestTimeout)
			if s.handleRequest(session, req) {
				return
			}
		case <-timer.C:
			log.Printf("[SSH] Session timed out waiting for request")
			return
		}
	}
}

// sshSession is the state of an SSH session until its command runs.
type sshSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	ch     ssh.Channel
	reqs   <-chan *ssh.Request
	// gitProtocol is the GIT_PROTOCOL the client sent, if accepted (see acceptedEnv)
	gitProtocol string
}

// acceptedEnv reports whether a client may set an environment variable for the git command it runs:
// only GIT_PROTOCOL selecting protocol v2, exactly as git sends it, so that a client can never change
// how git runs on the server (GIT_DIR, GIT_CONFIG_PARAMETERS, LD_PRELOAD, ...).
func acceptedEnv(name, value string) bool {
	return name == "GIT_PROTOCOL" && value == "version=2"
}

// handleRequest handles a request of a session, and reports whether the session is over: after the
// command it runs, or a malformed one. Replies only fail once the client is gone.
func (s *SSHServer) handleRequest(session *sshSession, req *ssh.Request) bool {
	ch, reqs := session.ch, session.reqs
	switch req.Type {
	case "exec":
		var payload struct {
			Value string
		}
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			log.Printf("[SSH] Failed to unmarshal exec payload: %v", err)
			_ = req.Reply(false, nil)
			return true
		}
		_ = req.Reply(true, nil)

		// When the client disconnects or closes the session, the reqs channel is closed by ssh.
		go func() {
			for range reqs {
			}
			session.cancel()
		}()

		err := s.executeGitCommand(session.ctx, ch, payload.Value, session.gitProtocol)
		sendExitStatus(ch, err)
		return true

	case "env":
		// Only an accepted variable is applied: the others are refused, not silently dropped
		var payload struct {
			Name, Value string
		}
		accepted := ssh.Unmarshal(req.Payload, &payload) == nil && acceptedEnv(payload.Name, payload.Value)
		if accepted {
			session.gitProtocol = payload.Value
		}
		_ = req.Reply(accepted, nil)

	default:
		_ = req.Reply(false, nil)
	}
	return false
}

// executeGitCommand runs a client's git command, with the git protocol it selected (protocol v2:
// "version=2", see acceptedEnv), or git's default.
func (s *SSHServer) executeGitCommand(ctx context.Context, ch ssh.Channel, rawCmd, gitProtocol string) error {
	log.Printf("[SSH] Command: %q", rawCmd)

	verb, repoPath, err := parseGitCommand(rawCmd)
	if err != nil {
		writeError(ch, err)
		return err
	}

	info, err := resolver.ParsePath(repoPath, s.cfg)
	if err != nil {
		writeError(ch, err)
		return err
	}

	// ParsePath guarantees ServingPath resides within the storage repos directory
	absServingPath, err := filepath.Abs(info.ServingPath)
	if err != nil {
		writeError(ch, err)
		return err
	}

	// Ensure the repository is cloned & converted with explicit user-friendly progress logs sent to client's SSH stderr
	log.Printf("[SSH] Ensuring repository %s is available & converted...", info.RepoName)
	_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: Checking cache for %s...\n", info.RepoName)

	// An existing copy is served while it is updated; a first conversion keeps the client waiting,
	// as the progress written to it keeps the connection alive
	err = s.converter.EnsureRepoWithin(ctx, info, ch.Stderr(), s.cfg.RequestWait)
	if errors.Is(err, converter.ErrConversionInProgress) {
		err = s.converter.EnsureRepo(ctx, info, ch.Stderr())
	}
	if err != nil {
		log.Printf("[SSH] JIT conversion failed for %s: %v", info.RepoName, err)
		if errors.Is(err, converter.ErrServerBusy) {
			_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: error: server busy with other conversions, please retry shortly\n")
		}
		if errors.Is(err, converter.ErrSubmoduleNotApproved) {
			_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: error: submodules need administrator approval, see the message below\n")
		}
		if errors.Is(err, converter.ErrSubmoduleUnavailable) {
			_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: error: a submodule could not be converted, please retry later\n")
		}
		if errors.Is(err, converter.ErrUpstreamObjectFormatChanged) {
			_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: error: the upstream changed its object format (e.g. to SHA-256), ask the administrator\n")
		}
		writeError(ch, fmt.Errorf("gitconv failed: %w", err))
		return err
	}

	// Execute git command directly against the safe path
	cmd := exec.CommandContext(ctx, verb, absServingPath)
	cmd.Env = converter.GitEnv()
	if acceptedEnv("GIT_PROTOCOL", gitProtocol) {
		cmd.Env = append(cmd.Env, "GIT_PROTOCOL="+gitProtocol)
	}
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()

	return cmd.Run()
}

func parseGitCommand(cmdStr string) (verb, repoPath string, err error) {
	parts := strings.SplitN(strings.TrimSpace(cmdStr), " ", 2)
	if len(parts) < 2 {
		return "", "", errors.New("invalid command format")
	}

	verb = parts[0]
	if verb != "git-upload-pack" {
		return "", "", fmt.Errorf("unsupported command: %s (Evergit is read-only)", verb)
	}

	repoPath = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
	return verb, repoPath, nil
}

func sendExitStatus(ch ssh.Channel, err error) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, exitStatus(err))
	_, _ = ch.SendRequest("exit-status", false, payload)
}

// exitStatus returns the exit status of a command: its own, or 1 if it failed without one.
func exitStatus(err error) uint32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return uint32(waitStatus.ExitStatus())
		}
	}
	return 1
}

func writeError(ch ssh.Channel, err error) {
	_, _ = fmt.Fprintf(ch.Stderr(), "error: %v\n", err)
}

func getOrCreateHostKey(path string) (ssh.Signer, error) {
	// Ensure parent directory of host key exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	// Only generate a key when none exists; replacing an unreadable one would change the host identity
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("failed to read SSH host key %s: %w", path, err)
	}

	log.Println("[SSH] Generating new Ed25519 host key...")
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	privateKeyPEM, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, fs.ErrExist) {
		// Another process created the key concurrently; use theirs
		return getOrCreateHostKey(path)
	}
	if err != nil {
		return nil, err
	}

	// convert the in-memory pem Block and save the pem file on disk
	if err := pem.Encode(f, privateKeyPEM); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}

	return ssh.NewSignerFromKey(privKey)
}
