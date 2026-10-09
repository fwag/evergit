package server

import (
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
	_ = nConn.SetDeadline(time.Time{})
	defer conn.Close()

	log.Printf("[SSH] New session established from %s", conn.RemoteAddr())

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported channel type")
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

func (s *SSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct {
				Value string
			}
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				log.Printf("[SSH] Failed to unmarshal exec payload: %v", err)
				req.Reply(false, nil)
				return
			}

			req.Reply(true, nil)

			err := s.executeGitCommand(ch, payload.Value)
			sendExitStatus(ch, err)
			return

		case "env":
			// Environment variables are not applied, so do not claim otherwise
			req.Reply(false, nil)

		default:
			req.Reply(false, nil)
		}
	}
}

func (s *SSHServer) executeGitCommand(ch ssh.Channel, rawCmd string) error {
	log.Printf("[SSH] Command: %s", rawCmd)

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

	if _, err := os.Stat(filepath.Join(info.ServingPath, "HEAD")); os.IsNotExist(err) {
		_, _ = fmt.Fprintf(ch.Stderr(), "remote: Evergit: First-time clone. Performing JIT SHA-1 -> SHA-256 history conversion, please wait...\n")
	}

	if err := s.converter.EnsureRepo(info, ch.Stderr()); err != nil {
		log.Printf("[SSH] JIT conversion failed for %s: %v", info.RepoName, err)
		writeError(ch, fmt.Errorf("gitconv failed: %w", err))
		return err
	}

	// Execute git command directly against the safe path
	cmd := exec.Command(verb, absServingPath)
	cmd.Env = converter.GitEnv()
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
	status := uint32(0)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				status = uint32(waitStatus.ExitStatus())
			} else {
				status = 1
			}
		} else {
			status = 1
		}
	}

	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, status)
	_, _ = ch.SendRequest("exit-status", false, payload)
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
