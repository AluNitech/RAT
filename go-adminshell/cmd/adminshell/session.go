package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"

	pb "modernrat-client/gen"

	"golang.org/x/term"
	"google.golang.org/grpc/metadata"
)

type adminSession struct {
	stream    pb.RemoteShellService_AdminShellClient
	userID    string
	sessionID string
	app       *adminApp

	termState *term.State
	rawMode   bool

	ctx        context.Context
	cancel     context.CancelFunc
	sendMu     sync.Mutex
	sendClosed bool
	sigCh      chan os.Signal

	ttyIn   *os.File
	ttyFD   int
	stdinWG sync.WaitGroup

	closed sync.Once
}

func (a *adminApp) runShell(userID string) error {
	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	ctxWithToken := metadata.NewOutgoingContext(a.ctx, md)

	stream, err := a.shellClient.AdminShell(ctxWithToken)
	if err != nil {
		return fmt.Errorf("AdminShell ストリーム開始失敗: %w", err)
	}
	session := newAdminSession(a, ctxWithToken, stream, userID)
	defer session.close()

	cols, rows := int32(0), int32(0)
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		cols, rows = int32(w), int32(h)
	}

	openMsg := &pb.ShellMessage{
		Type:   pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN,
		UserId: userID,
		Text:   "admin shell request",
		Cols:   cols,
		Rows:   rows,
	}
	if err := session.safeSend(openMsg); err != nil {
		return fmt.Errorf("OPEN 送信失敗: %w", err)
	}

	return session.receiveLoop()
}

func (s *adminSession) receiveLoop() error {
	readerStarted := false
	resizeWatcherStarted := false

	for {
		msg, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("AdminShell 受信エラー: %w", err)
		}

		switch msg.GetType() {
		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_OPEN:
			s.sessionID = msg.GetSessionId()
			log.Printf("セッション開始: session=%s", s.sessionID)

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_ACCEPTED:
			if msg.GetSessionId() != "" {
				s.sessionID = msg.GetSessionId()
			}
			log.Printf("クライアントがシェルを開始しました (session=%s)", s.sessionID)

			if !readerStarted {
				readerStarted = true
				if err := s.enableRawMode(); err != nil {
					log.Printf("警告: raw mode 設定に失敗しました: %v", err)
				}
				s.stdinWG.Add(1)
				go func(sessionID string) {
					defer s.stdinWG.Done()
					if err := s.stdinLoop(sessionID); err != nil {
						if !errors.Is(err, io.EOF) {
							log.Printf("STDIN 送信エラー: %v", err)
						}
						s.closeSession("stdin error")
					}
				}(s.sessionID)
			}

			if !resizeWatcherStarted {
				resizeWatcherStarted = true
				go s.watchResize()
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDOUT:
			if _, err := os.Stdout.Write(msg.GetData()); err != nil {
				return fmt.Errorf("STDOUT 書き込みに失敗: %w", err)
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDERR:
			if _, err := os.Stderr.Write(msg.GetData()); err != nil {
				return fmt.Errorf("STDERR 書き込みに失敗: %w", err)
			}

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_ERROR:
			return fmt.Errorf("セッションエラー: %s", msg.GetText())

		case pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE:
			fmt.Fprintf(os.Stderr, "\n[session closed] exit_code=%d reason=%s\n", msg.GetExitCode(), msg.GetText())
			return nil

		default:
			log.Printf("未知のメッセージタイプ: %v", msg.GetType())
		}
	}
}
func (s *adminSession) stdinLoop(sessionID string) error {
	if sessionID == "" {
		return errors.New("session id not initialized")
	}

	reader := s.inputFile()
	buf := make([]byte, 4096)
	var remoteBuf []byte

	// Escape sequence capture for correct pass-through and selective filtering
	type escType int
	const (
		escNone escType = iota
		escCSI
		escOSC
		escSS3
	)
	inEscape := false
	curEscType := escNone
	escSeq := make([]byte, 0, 64) // buffer for a single escape sequence

	flushRemote := func() error {
		if len(remoteBuf) == 0 {
			return nil
		}
		chunk := append([]byte(nil), remoteBuf...)
		remoteBuf = remoteBuf[:0]
		if sendErr := s.safeSend(&pb.ShellMessage{
			Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDIN,
			SessionId: sessionID,
			UserId:    s.userID,
			Data:      chunk,
		}); sendErr != nil {
			return sendErr
		}
		return nil
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		n, err := reader.Read(buf)
		if n > 0 {
			for i := 0; i < n; i++ {
				b := buf[i]
				// Escape sequence capture state machine (pass-through, with filtering)
				if !inEscape {
					if b == 0x1b { // ESC
						inEscape = true
						curEscType = escNone
						escSeq = escSeq[:0]
						escSeq = append(escSeq, b)
						continue
					}
				} else {
					// already in escape: accumulate and detect termination
					escSeq = append(escSeq, b)
					if len(escSeq) == 2 && b == '[' {
						curEscType = escCSI
						continue
					}
					if len(escSeq) == 2 && b == ']' {
						curEscType = escOSC
						continue
					}
					if len(escSeq) == 2 && b == 'O' {
						curEscType = escSS3
						// SS3 typically ends on this char or after one more; forward as-is
						// Treat next byte (if any) as part of the sequence then finish
						// We'll terminate on next iteration if printable
					}

					switch curEscType {
					case escCSI:
						// CSI ends when final byte in 0x40..0x7E appears
						if b >= 0x40 && b <= 0x7e {
							// Selectively DROP focus in/out reports: ESC [ ... I / ESC [ ... O
							drop := false
							if len(escSeq) >= 3 && escSeq[0] == 0x1b && escSeq[1] == '[' {
								final := escSeq[len(escSeq)-1]
								if final == 'I' || final == 'O' {
									drop = true
									for _, c := range escSeq[2 : len(escSeq)-1] {
										if (c >= '0' && c <= '9') || c == ';' || c == '?' {
											continue
										}
										drop = false
										break
									}
								}
							}
							if !drop {
								remoteBuf = append(remoteBuf, escSeq...)
							}
							inEscape = false
							curEscType = escNone
							escSeq = escSeq[:0]
						}
						continue
					case escOSC:
						// OSC ends on BEL or ESC \
						if b == 0x07 { // BEL
							remoteBuf = append(remoteBuf, escSeq...)
							inEscape = false
							curEscType = escNone
							escSeq = escSeq[:0]
						} else if b == 0x1b {
							// may be ESC \
							// leave accumulation; check next byte on next iteration
						} else if len(escSeq) >= 2 && escSeq[len(escSeq)-2] == 0x1b && b == '\\' {
							remoteBuf = append(remoteBuf, escSeq...)
							inEscape = false
							curEscType = escNone
							escSeq = escSeq[:0]
						}
						continue
					case escSS3:
						// SS3 sequences are short; when third byte arrives, forward and end
						if len(escSeq) >= 3 {
							remoteBuf = append(remoteBuf, escSeq...)
							inEscape = false
							curEscType = escNone
							escSeq = escSeq[:0]
						}
						continue
					default:
						// Unknown escape: if grows too large, flush defensively
						if len(escSeq) > cap(escSeq)-4 {
							remoteBuf = append(remoteBuf, escSeq...)
							inEscape = false
							curEscType = escNone
							escSeq = escSeq[:0]
						}
						continue
					}
				}

				// Forward raw byte
				remoteBuf = append(remoteBuf, b)
			}
			if err := flushRemote(); err != nil {
				return err
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = flushRemote()
				return s.safeSend(&pb.ShellMessage{
					Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE,
					SessionId: sessionID,
					UserId:    s.userID,
					Text:      "stdin closed",
				})
			}
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func (s *adminSession) closeSession(reason string) {
	s.closed.Do(func() {
		if s.sessionID == "" {
			return
		}
		_ = s.safeSend(&pb.ShellMessage{
			Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_CLOSE,
			SessionId: s.sessionID,
			UserId:    s.userID,
			Text:      reason,
		})
	})
}

func (s *adminSession) enableRawMode() error {
	fd := s.ensureTTY()
	if fd == 0 || !term.IsTerminal(fd) {
		return nil
	}

	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	s.termState = state
	s.rawMode = true

	// Explicitly disable xterm focus reporting to avoid stray CSI I/O sequences
	if s.ttyIn != nil {
		_, _ = s.ttyIn.Write([]byte(disableFocusSeq))
	} else {
		fmt.Fprint(os.Stdout, disableFocusSeq)
	}
	return nil
}

func (s *adminSession) restoreTerminal() {
	if !s.rawMode {
		return
	}

	fd := s.ttyFD
	if fd == 0 {
		fd = int(os.Stdin.Fd())
	}
	if s.termState != nil {
		if err := term.Restore(fd, s.termState); err != nil {
			log.Printf("警告: 端末状態の復元に失敗しました: %v", err)
		}
	}

	s.rawMode = false
	fmt.Fprint(os.Stderr, "\r\n")
}

func newAdminSession(app *adminApp, parent context.Context, stream pb.RemoteShellService_AdminShellClient, userID string) *adminSession {
	ctx, cancel := context.WithCancel(parent)
	return &adminSession{
		stream: stream,
		userID: userID,
		app:    app,
		ctx:    ctx,
		cancel: cancel,
	}
}

func (s *adminSession) safeSend(msg *pb.ShellMessage) error {
	s.sendMu.Lock()
	if s.sendClosed {
		s.sendMu.Unlock()
		return io.EOF
	}

	err := s.stream.Send(msg)
	if err != nil {
		s.sendClosed = true
	}
	s.sendMu.Unlock()

	if err != nil && s.cancel != nil {
		s.cancel()
	}

	return err
}

func (s *adminSession) close() {
	s.sendMu.Lock()
	if s.sendClosed {
		s.sendMu.Unlock()
		return
	}
	s.sendClosed = true
	s.sendMu.Unlock()

	if s.sigCh != nil {
		signal.Stop(s.sigCh)
	}
	if s.ttyIn != nil {
		_ = s.ttyIn.Close()
		s.ttyIn = nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.stream.CloseSend()
	s.restoreTerminal()
	s.stdinWG.Wait()
}

func (s *adminSession) ensureTTY() int {
	if s.ttyFD != 0 {
		return s.ttyFD
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		s.ttyIn = tty
		s.ttyFD = int(tty.Fd())
		return s.ttyFD
	}
	s.ttyFD = int(os.Stdin.Fd())
	return s.ttyFD
}

func (s *adminSession) inputFile() *os.File {
	if s.ensureTTY() != 0 && s.ttyIn != nil {
		return s.ttyIn
	}
	return os.Stdin
}
