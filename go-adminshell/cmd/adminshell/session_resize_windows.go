//go:build windows

package main

import (
	"os"
	"time"

	pb "modernrat-client/gen"

	"golang.org/x/term"
)

func (s *adminSession) watchResize() {
	if s.sessionID == "" {
		return
	}

	fd := int(os.Stdout.Fd())
	if fd == 0 {
		return
	}

	sendSize := func(cols, rows int) {
		_ = s.safeSend(&pb.ShellMessage{
			Type:      pb.ShellMessageType_SHELL_MESSAGE_TYPE_RESIZE,
			SessionId: s.sessionID,
			UserId:    s.userID,
			Cols:      int32(cols),
			Rows:      int32(rows),
		})
	}

	prevW, prevH := 0, 0
	updateSize := func() {
		w, h, err := term.GetSize(fd)
		if err != nil || w <= 0 || h <= 0 {
			return
		}
		if w == prevW && h == prevH {
			return
		}
		prevW, prevH = w, h
		sendSize(w, h)
	}

	updateSize()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			updateSize()
		}
	}
}
