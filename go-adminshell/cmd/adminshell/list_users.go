package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	pb "modernrat-client/gen"

	"google.golang.org/grpc/metadata"
)

func (a *adminApp) listUsers() error {
	if a.listPageSize <= 0 {
		a.listPageSize = defaultListPageSize
	}

	page := 1

	for {
		resp, err := a.fetchUsers(page, a.listPageSize, a.listFilter)
		if err != nil {
			return err
		}

		totalCount := int(resp.GetTotalCount())
		totalPages := calcTotalPages(totalCount, a.listPageSize)

		if totalCount > 0 && page > totalPages {
			page = totalPages
			resp, err = a.fetchUsers(page, a.listPageSize, a.listFilter)
			if err != nil {
				return err
			}
			totalCount = int(resp.GetTotalCount())
			totalPages = calcTotalPages(totalCount, a.listPageSize)
		} else if totalCount == 0 {
			page = 1
		}

		renderUserTable(resp.GetUsers(), page, a.listPageSize, totalCount, a.listFilter)

		prompt := "次の操作 [n]ext/[p]rev/[g]oto/[f]ilter/[s]ize/[q]uit: "
		if totalCount == 0 {
			prompt = "次の操作 [f]ilter/[s]ize/[q]uit: "
		}

		action, err := readLine(prompt)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		action = strings.TrimSpace(action)
		if action == "" {
			continue
		}

		lower := strings.ToLower(action)

		switch {
		case lower == "n" || lower == "next":
			if totalCount == 0 || page >= totalPages {
				fmt.Println("最終ページです。")
			} else {
				page++
			}

		case lower == "p" || lower == "prev":
			if page <= 1 {
				fmt.Println("最初のページです。")
			} else {
				page--
			}

		case lower == "f" || lower == "filter":
			fmt.Println("フィルタ例: online / offline / status:online / user:example")
			value, err := readLine("フィルタ文字列 (空で解除): ")
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			a.listFilter = strings.TrimSpace(value)
			page = 1

		case lower == "s" || lower == "size":
			value, err := readLine(fmt.Sprintf("ページサイズ (現在 %d): ", a.listPageSize))
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			size, convErr := strconv.Atoi(value)
			if convErr != nil || size <= 0 {
				fmt.Println("正の整数を入力してください。")
				continue
			}
			if size > 500 {
				size = 500
			}
			a.listPageSize = size
			page = 1

		case lower == "g" || lower == "goto":
			target, err := readLine("ジャンプ先ページ番号: ")
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			target = strings.TrimSpace(target)
			if target == "" {
				continue
			}
			targetPage, convErr := strconv.Atoi(target)
			if convErr != nil || targetPage <= 0 {
				fmt.Println("正の整数を入力してください。")
				continue
			}
			if totalCount == 0 {
				page = 1
			} else {
				if targetPage > totalPages {
					targetPage = totalPages
				}
				page = targetPage
			}

		case isAllDigits(lower):
			targetPage, convErr := strconv.Atoi(lower)
			if convErr != nil || targetPage <= 0 {
				fmt.Println("正の整数を入力してください。")
				continue
			}
			if totalCount == 0 {
				page = 1
			} else {
				if targetPage > totalPages {
					targetPage = totalPages
				}
				page = targetPage
			}

		case lower == "q" || lower == "quit" || lower == "exit":
			return nil

		default:
			fmt.Println("未対応の入力です。n/p/g/f/s/q を入力してください。")
		}
	}
}

func (a *adminApp) fetchUsers(page, pageSize int, filter string) (*pb.ListUsersResponse, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	ctx = metadata.NewOutgoingContext(ctx, md)

	req := &pb.ListUsersRequest{
		Page:     int32(page),
		PageSize: int32(pageSize),
		Filter:   filter,
	}

	resp, err := a.adminClient.ListUsers(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.GetSuccess() {
		msg := resp.GetMessage()
		if msg == "" {
			msg = "一覧取得に失敗しました"
		}
		return nil, errors.New(msg)
	}

	return resp, nil
}

func renderUserTable(users []*pb.UserSummary, page, pageSize int, totalCount int, filter string) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}

	totalPages := calcTotalPages(totalCount, pageSize)
	startIndex := (page-1)*pageSize + 1

	if totalCount == 0 {
		fmt.Println("ユーザーは見つかりませんでした。")
		fmt.Println()
		fmt.Printf("ページ %d/%d  表示 0 件 (ページサイズ %d)", page, totalPages, pageSize)
		if filter != "" {
			fmt.Printf("  フィルタ: %s", filter)
		}
		fmt.Println()
		return
	}

	headers := []string{"#", "STATUS", "USER ID", "USERNAME", "IP", "HOSTNAME", "OS", "LAST SEEN", "SEEN AGO", "REGISTERED"}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	rows := make([][]string, len(users))
	for i, u := range users {
		row := []string{
			fmt.Sprintf("%d", startIndex+i),
			formatStatus(u.GetIsOnline()),
			valueOrDash(u.GetUserId()),
			valueOrDash(u.GetUsername()),
			valueOrDash(u.GetIpAddress()),
			valueOrDash(u.GetHostname()),
			valueOrDash(u.GetOsName()),
			formatTimestampShort(u.GetLastSeen()),
			formatRelative(u.GetLastSeen()),
			formatDate(u.GetRegisteredAt()),
		}
		rows[i] = row
		for j, cell := range row {
			if len(cell) > widths[j] {
				widths[j] = len(cell)
			}
		}
	}

	printTableSeparator(widths)
	printTableRow(headers, widths)
	printTableSeparator(widths)
	for _, row := range rows {
		printTableRow(row, widths)
	}
	printTableSeparator(widths)

	endIndex := startIndex + len(users) - 1
	fmt.Printf("ページ %d/%d  表示 %d-%d / %d件 (ページサイズ %d)", page, totalPages, startIndex, endIndex, totalCount, pageSize)
	if filter != "" {
		fmt.Printf("  フィルタ: %s", filter)
	}
	fmt.Println()
}

func printTableSeparator(widths []int) {
	var b strings.Builder
	b.WriteByte('+')
	for _, w := range widths {
		b.WriteString(strings.Repeat("-", w+2))
		b.WriteByte('+')
	}
	fmt.Println(b.String())
}

func printTableRow(row []string, widths []int) {
	var b strings.Builder
	b.WriteByte('|')
	for i, cell := range row {
		pad := widths[i]
		if pad < 0 {
			pad = 0
		}
		fmt.Fprintf(&b, " %-*s ", pad, cell)
		b.WriteByte('|')
	}
	fmt.Println(b.String())
}

func calcTotalPages(totalCount, pageSize int) int {
	if pageSize <= 0 {
		return 1
	}
	pages := totalCount / pageSize
	if totalCount%pageSize != 0 {
		pages++
	}
	if pages <= 0 {
		pages = 1
	}
	return pages
}

func formatStatus(isOnline bool) string {
	if isOnline {
		return "ONLINE"
	}
	return "OFFLINE"
}

func valueOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func formatTimestampShort(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

func formatRelative(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	t := time.Unix(ts, 0)
	delta := time.Since(t)
	if delta < 0 {
		delta = -delta
		return "in " + humanizeDuration(delta)
	}
	if delta < 5*time.Second {
		return "just now"
	}
	return humanizeDuration(delta) + " ago"
}

func formatDate(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02")
}

func humanizeDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		hours := int(d / time.Hour)
		return fmt.Sprintf("%dh", hours)
	case d >= time.Minute:
		minutes := int(d / time.Minute)
		return fmt.Sprintf("%dm", minutes)
	case d >= time.Second:
		seconds := int(d / time.Second)
		return fmt.Sprintf("%ds", seconds)
	default:
		return "0s"
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
