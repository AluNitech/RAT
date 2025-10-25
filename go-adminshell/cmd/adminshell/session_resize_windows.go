//go:build windows

package main

import (
	"os"

	pb "modernrat-client/gen"

	"golang.org/x/term"
)

func (s *adminSession) watchResize() {
	if s.sessionID == "" {
		return
	}

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
