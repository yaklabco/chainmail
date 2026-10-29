// Package client connects to the chainmail daemon over its Unix socket.
package client

import (
	"context"
	"net"
	"net/http"

	"connectrpc.com/connect"

	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// Client holds one Connect client per daemon service.
type Client struct {
	Account chainmailv1connect.AccountServiceClient
	Admin   chainmailv1connect.AdminServiceClient
}

// New returns a client for the daemon listening on socketPath. kind names the
// client in the audit log, such as "cli" or "mcp".
func New(socketPath, kind string) *Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	hc := &http.Client{Transport: transport}
	opts := connect.WithInterceptors(clientHeader(kind))
	// The host is ignored: every request goes to the socket.
	const baseURL = "http://chainmail.local"
	return &Client{
		Account: chainmailv1connect.NewAccountServiceClient(hc, baseURL, opts),
		Admin:   chainmailv1connect.NewAdminServiceClient(hc, baseURL, opts),
	}
}

func clientHeader(kind string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(api.HeaderClient, kind)
			return next(ctx, req)
		}
	}
}
