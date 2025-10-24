package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "modernrat-client/gen"

	"google.golang.org/grpc/metadata"
)

func (a *adminApp) openFileTransferStream() (pb.FileTransferService_AdminFileTransferClient, context.CancelFunc, error) {
	if a.fileClient == nil {
		return nil, nil, errors.New("file transfer service client is not initialized")
	}

	ctx, cancel := context.WithCancel(a.ctx)
	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	ctx = metadata.NewOutgoingContext(ctx, md)

	stream, err := a.fileClient.AdminFileTransfer(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}

	return stream, cancel, nil
}

func (a *adminApp) performUpload(userID, localPath, remotePath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("ローカルファイルの確認に失敗しました: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("通常ファイル以外はアップロードできません: %s", localPath)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("ローカルファイルのオープンに失敗しました: %w", err)
	}
	defer file.Close()

	stream, cancel, err := a.openFileTransferStream()
	if err != nil {
		return fmt.Errorf("ファイル転送ストリーム開始失敗: %w", err)
	}

	msgCh := make(chan *pb.FileTransferMessage, 8)
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		defer close(msgCh)
		for {
			msg, recvErr := stream.Recv()
			if recvErr != nil {
				select {
				case errCh <- recvErr:
				default:
				}
				return
			}
			select {
			case msgCh <- msg:
			case <-a.ctx.Done():
				return
			}
		}
	}()

	defer func() {
		cancel()
		_ = stream.CloseSend()
		<-doneCh
	}()

	request := &pb.FileTransferMessage{
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_UPLOAD_REQUEST,
		UserId:     userID,
		RemotePath: remotePath,
		FileSize:   info.Size(),
	}
	if err := stream.Send(request); err != nil {
		return fmt.Errorf("アップロード要求の送信に失敗しました: %w", err)
	}

	var (
		transferID string
		pending    []*pb.FileTransferMessage
	)

waitAccepted:
	for {
		select {
		case msg, ok := <-msgCh:
			if !ok {
				return errors.New("ファイル転送ストリームが予期せず終了しました")
			}
			switch msg.GetType() {
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED:
				transferID = msg.GetTransferId()
				log.Printf("アップロード開始: user=%s transfer=%s size=%d remote=%s", userID, transferID, info.Size(), remotePath)
				break waitAccepted
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED:
				return fmt.Errorf("クライアントに拒否されました: %s", msg.GetText())
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
				return fmt.Errorf("ファイル転送エラー: %s", msg.GetText())
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
				return fmt.Errorf("ファイル転送がキャンセルされました: %s", msg.GetText())
			default:
				pending = append(pending, msg)
			}
		case recvErr := <-errCh:
			if recvErr != nil {
				return fmt.Errorf("ファイル転送受信に失敗しました: %w", recvErr)
			}
		case <-a.ctx.Done():
			return a.ctx.Err()
		}
	}

	if transferID == "" {
		return errors.New("transfer id が取得できませんでした")
	}

	progress := newProgressPrinter("upload", remotePath, info.Size())
	const chunkSize = 64 * 1024
	buf := make([]byte, chunkSize)
	var offset int64

	for {
		if err := checkTransferInterrupt(a.ctx, msgCh, errCh, &pending); err != nil {
			progress.finish(false)
			return err
		}

		n, readErr := file.Read(buf)
		if n > 0 {
			chunk := &pb.FileTransferMessage{
				Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA,
				TransferId: transferID,
				UserId:     userID,
				Offset:     offset,
				Data:       append([]byte(nil), buf[:n]...),
			}
			if err := stream.Send(chunk); err != nil {
				progress.finish(false)
				return fmt.Errorf("データ送信に失敗しました: %w", err)
			}
			offset += int64(n)
			progress.advance(int64(n))
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			progress.finish(false)
			return fmt.Errorf("ローカルファイルの読み込みに失敗しました: %w", readErr)
		}
	}

	complete := &pb.FileTransferMessage{
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE,
		TransferId: transferID,
		UserId:     userID,
		RemotePath: remotePath,
	}
	if err := stream.Send(complete); err != nil {
		progress.finish(false)
		return fmt.Errorf("COMPLETE メッセージ送信に失敗しました: %w", err)
	}

	if err := waitForTransferCompletion(a.ctx, msgCh, errCh, &pending); err != nil {
		progress.finish(false)
		return err
	}

	progress.finish(true)
	log.Printf("アップロード完了: user=%s transfer=%s bytes=%d", userID, transferID, offset)
	return nil
}

func (a *adminApp) performDownload(userID, remotePath, localPath string) error {
	stream, cancel, err := a.openFileTransferStream()
	if err != nil {
		return fmt.Errorf("ファイル転送ストリーム開始失敗: %w", err)
	}
	defer cancel()
	defer func() {
		_ = stream.CloseSend()
	}()

	request := &pb.FileTransferMessage{
		Type:       pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DOWNLOAD_REQUEST,
		UserId:     userID,
		RemotePath: remotePath,
	}
	if err := stream.Send(request); err != nil {
		return fmt.Errorf("ダウンロード要求の送信に失敗しました: %w", err)
	}

	var (
		transferID string
		file       *os.File
		received   int64
		totalSize  int64
		progress   *progressPrinter
	)

	cancelWithError := func(err error) error {
		if file != nil {
			file.Close()
			file = nil
			_ = os.Remove(localPath)
		}
		if progress != nil {
			progress.finish(false)
			progress = nil
		}
		return err
	}

	finalizeSuccess := func() {
		if file != nil {
			file.Close()
			file = nil
		}
		if progress != nil {
			progress.finish(true)
			progress = nil
		}
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if transferID == "" {
					return cancelWithError(errors.New("ダウンロードが開始される前にストリームが終了しました"))
				}
				finalizeSuccess()
				log.Printf("ダウンロード完了: user=%s transfer=%s bytes=%d 保存先=%s", userID, transferID, received, localPath)
				return nil
			}
			return cancelWithError(fmt.Errorf("ダウンロード中の受信に失敗しました: %w", err))
		}

		switch resp.GetType() {
		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED:
			transferID = resp.GetTransferId()
			totalSize = resp.GetFileSize()

			dir := filepath.Dir(localPath)
			if dir != "." && dir != "" {
				if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
					return cancelWithError(fmt.Errorf("保存先ディレクトリの作成に失敗しました: %w", mkErr))
				}
			}

			target, openErr := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if openErr != nil {
				return cancelWithError(fmt.Errorf("保存先ファイルのオープンに失敗しました: %w", openErr))
			}
			file = target
			received = 0
			log.Printf("ダウンロード開始: user=%s transfer=%s size=%d remote=%s local=%s", userID, transferID, totalSize, remotePath, localPath)
			progress = newProgressPrinter("download", remotePath, totalSize)

		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA:
			if file == nil {
				return cancelWithError(errors.New("ACCEPTED を受信する前に DATA を受信しました"))
			}
			data := resp.GetData()
			if len(data) == 0 {
				continue
			}
			if _, writeErr := file.Write(data); writeErr != nil {
				return cancelWithError(fmt.Errorf("ローカルファイルへの書き込みに失敗しました: %w", writeErr))
			}
			received += int64(len(data))
			if progress != nil {
				progress.advance(int64(len(data)))
			}

		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE:
			finalizeSuccess()
			log.Printf("ダウンロード完了: user=%s transfer=%s bytes=%d 保存先=%s", userID, transferID, received, localPath)
			return nil

		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_REJECTED:
			return cancelWithError(fmt.Errorf("クライアントに拒否されました: %s", resp.GetText()))

		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
			return cancelWithError(fmt.Errorf("ダウンロード中にエラーが発生しました: %s", resp.GetText()))

		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
			return cancelWithError(fmt.Errorf("ダウンロードがキャンセルされました: %s", resp.GetText()))

		default:
			log.Printf("想定外のメッセージを受信: type=%v", resp.GetType())
		}
	}
}

func checkTransferInterrupt(ctx context.Context, msgCh <-chan *pb.FileTransferMessage, errCh <-chan error, pending *[]*pb.FileTransferMessage) error {
	for {
		select {
		case msg, ok := <-msgCh:
			if !ok {
				return errors.New("ファイル転送ストリームが終了しました")
			}
			switch msg.GetType() {
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
				return fmt.Errorf("ファイル転送がキャンセルされました: %s", msg.GetText())
			case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
				return fmt.Errorf("ファイル転送エラー: %s", msg.GetText())
			default:
				*pending = append(*pending, msg)
			}
		case recvErr := <-errCh:
			if recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					return nil
				}
				return fmt.Errorf("ファイル転送受信エラー: %w", recvErr)
			}
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
}

func waitForTransferCompletion(ctx context.Context, msgCh <-chan *pb.FileTransferMessage, errCh <-chan error, pending *[]*pb.FileTransferMessage) error {
	process := func(msg *pb.FileTransferMessage) error {
		switch msg.GetType() {
		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_COMPLETE:
			return nil
		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ERROR:
			return fmt.Errorf("ファイル転送中にエラーが発生しました: %s", msg.GetText())
		case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_CANCEL:
			return fmt.Errorf("ファイル転送がキャンセルされました: %s", msg.GetText())
		default:
			return errUnexpectedTransferMessage(msg)
		}
	}

	for {
		if len(*pending) > 0 {
			msg := (*pending)[0]
			*pending = (*pending)[1:]
			if err := process(msg); err == nil {
				return nil
			} else if !errors.Is(err, errIgnoreMessage) {
				return err
			}
			continue
		}

		select {
		case msg, ok := <-msgCh:
			if !ok {
				return errors.New("ファイル転送ストリームが終了しました")
			}
			if err := process(msg); err != nil {
				if errors.Is(err, errIgnoreMessage) {
					continue
				}
				return err
			}
			return nil
		case recvErr := <-errCh:
			if recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					return nil
				}
				return fmt.Errorf("ファイル転送受信エラー: %w", recvErr)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var errIgnoreMessage = errors.New("ignore transfer message")

func errUnexpectedTransferMessage(msg *pb.FileTransferMessage) error {
	switch msg.GetType() {
	case pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_ACCEPTED,
		pb.FileTransferMessageType_FILE_TRANSFER_MESSAGE_TYPE_DATA:
		return errIgnoreMessage
	default:
		return fmt.Errorf("想定外のメッセージを受信: type=%v", msg.GetType())
	}
}

type progressPrinter struct {
	action      string
	path        string
	total       int64
	transferred int64
	lastRender  time.Time
	printed     bool
	done        bool
}

func newProgressPrinter(action, path string, total int64) *progressPrinter {
	return &progressPrinter{action: action, path: path, total: total}
}

func (p *progressPrinter) advance(delta int64) {
	if p == nil || p.done {
		return
	}
	p.transferred += delta
	p.render(false)
}

func (p *progressPrinter) finish(success bool) {
	if p == nil || p.done {
		return
	}
	if success && p.total > 0 && p.transferred < p.total {
		p.transferred = p.total
	}
	p.render(true)
	if p.printed {
		fmt.Println()
	}
	p.done = true
}

func (p *progressPrinter) render(force bool) {
	if p == nil || p.done {
		return
	}
	now := time.Now()
	if !force && !p.lastRender.IsZero() && now.Sub(p.lastRender) < 200*time.Millisecond {
		return
	}
	p.lastRender = now

	action := strings.ToUpper(p.action)
	shortPath := truncatePath(p.path, 32)

	if p.total > 0 {
		ratio := float64(p.transferred)
		if p.total > 0 {
			ratio /= float64(p.total)
		}
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		const barWidth = 24
		filled := int(ratio*float64(barWidth) + 0.5)
		if filled > barWidth {
			filled = barWidth
		}
		bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
		fmt.Printf("\r[%s] %-32s [%s] %6.2f%% (%d/%d bytes)", action, shortPath, bar, ratio*100, p.transferred, p.total)
	} else {
		fmt.Printf("\r[%s] %-32s %d bytes", action, shortPath, p.transferred)
	}
	p.printed = true
}

func truncatePath(path string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(path) <= max {
		return path
	}
	if max <= 3 {
		return path[:max]
	}
	return "..." + path[len(path)-(max-3):]
}
