package fnosbridge

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SocketTakeover moves the official listener to the upstream path and leaves
// the bridge on the canonical fnOS music socket. Close reverses the operation.
type SocketTakeover struct {
	Target   string
	Upstream string
	Listener net.Listener

	mu     sync.Mutex
	closed bool
	moved  bool
}

func TakeOver(target, upstream string, mode os.FileMode) (*SocketTakeover, error) {
	var err error
	target, err = filepath.Abs(strings.TrimSpace(target))
	if err != nil {
		return nil, fmt.Errorf("resolve target socket: %w", err)
	}
	upstream, err = filepath.Abs(strings.TrimSpace(upstream))
	if err != nil {
		return nil, fmt.Errorf("resolve upstream socket: %w", err)
	}
	if target == "." || upstream == "." || target == upstream {
		return nil, fmt.Errorf("target and upstream sockets must be distinct paths")
	}

	takeover := &SocketTakeover{Target: target, Upstream: upstream}
	if err := takeover.prepare(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", target)
	if err != nil {
		if rollbackErr := takeover.rollbackMoved(); rollbackErr != nil {
			return nil, fmt.Errorf("listen on fnOS music socket %s: %w; rollback failed: %v", target, err, rollbackErr)
		}
		return nil, fmt.Errorf("listen on fnOS music socket %s: %w", target, err)
	}
	takeover.Listener = listener
	if mode != 0 {
		if err := os.Chmod(target, mode); err != nil {
			_ = takeover.Close()
			return nil, fmt.Errorf("set fnOS music socket mode: %w", err)
		}
	}
	return takeover, nil
}

func (t *SocketTakeover) prepare() error {
	targetInfo, err := os.Lstat(t.Target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("official music socket %s does not exist; start fnOS Music first", t.Target)
		}
		return fmt.Errorf("inspect official music socket: %w", err)
	}
	if targetInfo.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing non-socket path %s", t.Target)
	}

	upstreamInfo, err := os.Lstat(t.Upstream)
	switch {
	case errors.Is(err, os.ErrNotExist):
		targetLive, probeErr := socketLive(t.Target)
		if probeErr != nil {
			return probeErr
		}
		if !targetLive {
			return fmt.Errorf("official music socket %s is not accepting connections; restart fnOS Music", t.Target)
		}
		if err := renameNoReplace(t.Target, t.Upstream); err != nil {
			return fmt.Errorf("move official music socket upstream: %w", err)
		}
		t.moved = true
		return nil
	case err != nil:
		return fmt.Errorf("inspect upstream music socket: %w", err)
	case upstreamInfo.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("refusing non-socket upstream path %s", t.Upstream)
	}

	upstreamLive, probeErr := socketLive(t.Upstream)
	if probeErr != nil {
		return probeErr
	}
	if !upstreamLive {
		return fmt.Errorf("upstream socket %s exists but is stale; restart fnOS Music and remove it only after confirming no local process owns it", t.Upstream)
	}
	targetLive, probeErr := socketLive(t.Target)
	if probeErr != nil {
		return probeErr
	}
	if targetLive {
		return fmt.Errorf("both %s and %s have live listeners; refusing ambiguous socket takeover", t.Target, t.Upstream)
	}
	if err := os.Remove(t.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale bridge socket: %w", err)
	}
	return nil
}

func (t *SocketTakeover) rollbackMoved() error {
	if !t.moved {
		return nil
	}
	if _, err := os.Lstat(t.Upstream); err != nil {
		return err
	}
	if info, err := os.Lstat(t.Target); err == nil && info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s during rollback", t.Target)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(t.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := renameNoReplace(t.Upstream, t.Target); err != nil {
		return err
	}
	t.moved = false
	return nil
}

func (t *SocketTakeover) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.Listener != nil {
		_ = t.Listener.Close()
	}

	info, err := os.Lstat(t.Target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect bridge socket during restore: %w", err)
	}
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to remove non-socket path %s during restore", t.Target)
		}
		live, probeErr := socketLive(t.Target)
		if probeErr != nil {
			return probeErr
		}
		if live {
			return fmt.Errorf("bridge socket %s is still live; refusing to remove it", t.Target)
		}
		if err := os.Remove(t.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove bridge socket: %w", err)
		}
	}
	if _, err := os.Lstat(t.Upstream); err != nil {
		return fmt.Errorf("official upstream socket missing during restore: %w", err)
	}
	if err := renameNoReplace(t.Upstream, t.Target); err != nil {
		return fmt.Errorf("restore official music socket: %w", err)
	}
	t.moved = false
	t.closed = true
	return nil
}

func socketLive(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, 400*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "connection refused") ||
		strings.Contains(message, "no such file") ||
		strings.Contains(message, "not found") {
		return false, nil
	}
	return false, fmt.Errorf("cannot determine whether socket %s is live: %w", path, err)
}
