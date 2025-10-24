package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	pb "modernrat-server/gen"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type transferDirection string

const (
	transferDirectionUpload   transferDirection = "upload"
	transferDirectionDownload transferDirection = "download"
)

func (s *server) AdminFileTransfer(stream pb.FileTransferService_AdminFileTransferServer) error {
	if err := s.authenticate(stream.Context()); err != nil {
		return err
	}

	adminConn := newFileAdminConnection(stream)
	defer adminConn.Close()

	defer s.cleanupAdminTransfers(adminConn, "admin stream closed")

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.cleanupAdminTransfers(adminConn, fmt.Sprintf("admin stream error: %v", err))
			return err
		}

		if err := s.handleAdminTransferMessage(stream.Context(), adminConn, msg); err != nil {
			if status.Code(err) == codes.Unavailable {
				return nil
			}
			return err
		}
	}
}

func (s *server) handleAdminTransferMessage(ctx context.Context, adminConn *fileAdminConnection, msg *pb.FileTransferMessage) error {
	switch msg.GetType() {
	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_UPLOAD_REQUEST,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DOWNLOAD_REQUEST:
		return s.startFileTransfer(ctx, adminConn, msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
		if msg.GetTransferId() == "" {
			return status.Error(codes.InvalidArgument, "transfer_id is required")
		}
		session, ok := s.fileHub.getSession(msg.GetTransferId())
		if !ok {
			return status.Error(codes.NotFound, "transfer session not found")
		}
		msg.UserId = session.userID
		if err := session.clientConn.Send(msg); err != nil {
			s.fileHub.endSession(session.id)
			notify := &pb.FileTransferMessage{
				TransferId: session.id,
				UserId:     session.userID,
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
				Text:       "failed to reach client agent",
			}
			_ = session.adminConn.Send(notify)
			return status.Error(codes.Unavailable, "client disconnected")
		}

		if msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL || msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE || msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR {
			s.fileHub.endSession(session.id)
		}
		return nil

	default:
		return status.Error(codes.InvalidArgument, "unsupported admin message type")
	}
}

func (s *server) startFileTransfer(ctx context.Context, adminConn *fileAdminConnection, msg *pb.FileTransferMessage) error {
	userID := strings.TrimSpace(msg.GetUserId())
	if userID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	remotePath := strings.TrimSpace(msg.GetRemotePath())
	if remotePath == "" {
		return status.Error(codes.InvalidArgument, "remote_path is required")
	}

	session, err := s.fileHub.startSession(userID, adminConn)
	if err != nil {
		notify := &pb.FileTransferMessage{
			Type:   pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
			UserId: userID,
			Text:   "target user is not connected",
		}
		_ = adminConn.Send(notify)
		return status.Error(codes.Unavailable, "target user is not connected")
	}

	msg.TransferId = session.id
	msg.UserId = userID

	if msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_UPLOAD_REQUEST && msg.GetFileSize() < 0 {
		msg.FileSize = 0
	}

	if err := session.clientConn.Send(msg); err != nil {
		s.fileHub.endSession(session.id)
		notify := &pb.FileTransferMessage{
			TransferId: session.id,
			UserId:     session.userID,
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
			Text:       fmt.Sprintf("failed to deliver transfer request: %v", err),
		}
		_ = adminConn.Send(notify)
		return status.Error(codes.Unavailable, "failed to reach client agent")
	}

	return nil
}

func (s *server) ClientFileTransfer(stream pb.FileTransferService_ClientFileTransferServer) error {
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}

	if first.GetType() != pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REGISTER {
		return status.Error(codes.InvalidArgument, "first message must be REGISTER")
	}

	userID := strings.TrimSpace(first.GetUserId())
	if userID == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}

	clientConn := newFileClientConnection(userID, stream)
	defer clientConn.Close()

	orphaned := s.fileHub.registerClient(clientConn)
	if len(orphaned) > 0 {
		for _, session := range orphaned {
			notify := &pb.FileTransferMessage{
				TransferId: session.id,
				UserId:     session.userID,
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
				Text:       "client reconnected",
			}
			_ = session.adminConn.Send(notify)
		}
	}

	ack := &pb.FileTransferMessage{
		Type:   pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REGISTERED,
		UserId: userID,
		Text:   "file transfer channel ready",
	}
	_ = clientConn.Send(ack)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("File transfer client stream error: user=%s err=%v", userID, err)
			break
		}

		if err := s.handleClientTransferMessage(clientConn, msg); err != nil {
			log.Printf("handle client transfer message failed: user=%s err=%v", userID, err)
		}
	}

	ended := s.fileHub.unregisterClient(clientConn)
	if len(ended) > 0 {
		for _, session := range ended {
			notify := &pb.FileTransferMessage{
				TransferId: session.id,
				UserId:     session.userID,
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
				Text:       "client disconnected",
			}
			_ = session.adminConn.Send(notify)
		}
	}

	return nil
}

func (s *server) handleClientTransferMessage(conn *fileClientConnection, msg *pb.FileTransferMessage) error {
	switch msg.GetType() {
	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
		if msg.GetTransferId() == "" {
			return errors.New("transfer_id missing in client message")
		}
		session, ok := s.fileHub.getSession(msg.GetTransferId())
		if !ok {
			return errors.New("transfer session not found for client message")
		}
		msg.UserId = session.userID
		if err := session.adminConn.Send(msg); err != nil {
			log.Printf("failed to send file transfer message to admin: transfer=%s err=%v", session.id, err)
		}

		if msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE || msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR || msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL || msg.GetType() == pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED {
			s.fileHub.endSession(session.id)
		}
		return nil

	default:
		return errors.New("unsupported client message type")
	}
}

func (s *server) cleanupAdminTransfers(adminConn *fileAdminConnection, reason string) {
	sessions := s.fileHub.endSessionsForAdmin(adminConn)
	if len(sessions) == 0 {
		return
	}

	for _, session := range sessions {
		msg := &pb.FileTransferMessage{
			TransferId: session.id,
			UserId:     session.userID,
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL,
			Text:       reason,
		}
		_ = session.clientConn.Send(msg)
	}
}
