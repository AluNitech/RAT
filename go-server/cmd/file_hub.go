package main

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "modernrat-server/gen"
)

type fileHub struct {
	mu       sync.RWMutex
	clients  map[string]*fileClientConnection
	sessions map[string]*fileTransferSession
}

func newFileHub() *fileHub {
	return &fileHub{
		clients:  make(map[string]*fileClientConnection),
		sessions: make(map[string]*fileTransferSession),
	}
}

type fileClientConnection struct {
	userID string
	stream pb.FileTransferService_ClientFileTransferServer

	sendCh    chan *pb.FileTransferMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newFileClientConnection(userID string, stream pb.FileTransferService_ClientFileTransferServer) *fileClientConnection {
	conn := &fileClientConnection{
		userID: userID,
		stream: stream,
		sendCh: make(chan *pb.FileTransferMessage, 32),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *fileClientConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *fileClientConnection) Send(msg *pb.FileTransferMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("client file connection closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *fileClientConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

type fileAdminConnection struct {
	stream pb.FileTransferService_AdminFileTransferServer

	sendCh    chan *pb.FileTransferMessage
	doneCh    chan struct{}
	closeOnce sync.Once
}

func newFileAdminConnection(stream pb.FileTransferService_AdminFileTransferServer) *fileAdminConnection {
	conn := &fileAdminConnection{
		stream: stream,
		sendCh: make(chan *pb.FileTransferMessage, 32),
		doneCh: make(chan struct{}),
	}

	go conn.sender()
	return conn
}

func (c *fileAdminConnection) sender() {
	defer close(c.doneCh)

	for msg := range c.sendCh {
		if err := c.stream.Send(msg); err != nil {
			return
		}
	}
}

func (c *fileAdminConnection) Send(msg *pb.FileTransferMessage) error {
	select {
	case <-c.doneCh:
		return errors.New("admin file connection closed")
	case c.sendCh <- msg:
		return nil
	}
}

func (c *fileAdminConnection) Close() {
	c.closeOnce.Do(func() {
		close(c.sendCh)
		<-c.doneCh
	})
}

type fileTransferSession struct {
	id     string
	userID string

	adminConn  *fileAdminConnection
	clientConn *fileClientConnection
	startedAt  time.Time
}

func (h *fileHub) registerClient(conn *fileClientConnection) (ended []*fileTransferSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[conn.userID]; ok && existing != conn {
		ended = h.detachSessionsLocked(existing)
	}

	h.clients[conn.userID] = conn
	return ended
}

func (h *fileHub) unregisterClient(conn *fileClientConnection) (ended []*fileTransferSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current, ok := h.clients[conn.userID]
	if !ok || current != conn {
		return nil
	}

	delete(h.clients, conn.userID)
	return h.detachSessionsLocked(conn)
}

func (h *fileHub) detachSessionsLocked(conn *fileClientConnection) (ended []*fileTransferSession) {
	for id, session := range h.sessions {
		if session.clientConn == conn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}
	return ended
}

func (h *fileHub) startSession(userID string, adminConn *fileAdminConnection) (*fileTransferSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clientConn, ok := h.clients[userID]
	if !ok {
		return nil, errors.New("user not connected")
	}

	id := uuid.NewString()
	session := &fileTransferSession{
		id:         id,
		userID:     userID,
		adminConn:  adminConn,
		clientConn: clientConn,
		startedAt:  time.Now(),
	}

	h.sessions[id] = session
	return session, nil
}

func (h *fileHub) getSession(id string) (*fileTransferSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	session, ok := h.sessions[id]
	return session, ok
}

func (h *fileHub) endSession(id string) (*fileTransferSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.sessions[id]
	if !ok {
		return nil, false
	}

	delete(h.sessions, id)
	return session, true
}

func (h *fileHub) endSessionsForAdmin(adminConn *fileAdminConnection) []*fileTransferSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	var ended []*fileTransferSession
	for id, session := range h.sessions {
		if session.adminConn == adminConn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}
	return ended
}

func (h *fileHub) endSessionsForClient(clientConn *fileClientConnection) []*fileTransferSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	var ended []*fileTransferSession
	for id, session := range h.sessions {
		if session.clientConn == clientConn {
			delete(h.sessions, id)
			ended = append(ended, session)
		}
	}
	return ended
}

func (h *fileHub) endAllSessions() []*fileTransferSession {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.sessions) == 0 {
		return nil
	}

	ended := make([]*fileTransferSession, 0, len(h.sessions))
	for id, session := range h.sessions {
		ended = append(ended, session)
		delete(h.sessions, id)
	}
	return ended
}
