package main

import (
	"context"
	"errors"
	"time"

	pb "modernrat-client/gen"
)

func requestAdminToken(ctx context.Context, client pb.AdminServiceClient, password string, ttl time.Duration) (string, time.Time, error) {
	req := &pb.GenerateAdminTokenRequest{Password: password}

	if ttl > 0 {
		seconds := int64(ttl / time.Second)
		if seconds <= 0 {
			seconds = 1
		}
		req.TtlSeconds = seconds
	}

	tokenCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := client.GenerateAdminToken(tokenCtx, req)
	if err != nil {
		return "", time.Time{}, err
	}
	if !resp.GetSuccess() {
		msg := resp.GetMessage()
		if msg == "" {
			msg = "token request rejected"
		}
		return "", time.Time{}, errors.New(msg)
	}

	var expiresAt time.Time
	if resp.GetExpiresAt() > 0 {
		expiresAt = time.Unix(resp.GetExpiresAt(), 0).UTC()
	}

	return resp.GetToken(), expiresAt, nil
}
