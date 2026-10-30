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

// maxMessageBytes is the largest unary message response accepted. The
// daemon caps its responses at 16 MiB; the rest is protocol overhead.
const maxMessageBytes = 17 << 20

// maxComposeBytes is the largest Compose response accepted, such as a dry
// run holding a whole message. The daemon caps Compose responses at 40 MiB;
// the rest is protocol overhead.
const maxComposeBytes = 41 << 20

// Client holds one Connect client per daemon service.
type Client struct {
	Account  chainmailv1connect.AccountServiceClient
	Admin    chainmailv1connect.AdminServiceClient
	Message  chainmailv1connect.MessageServiceClient
	Compose  chainmailv1connect.ComposeServiceClient
	Organize chainmailv1connect.OrganizeServiceClient

	transport *http.Transport
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
	opts := connect.WithInterceptors(clientHeader{kind: kind})
	// The host is ignored: every request goes to the socket.
	const baseURL = "http://chainmail.local"
	return &Client{
		transport: transport,
		Account:   chainmailv1connect.NewAccountServiceClient(hc, baseURL, opts),
		Admin:     chainmailv1connect.NewAdminServiceClient(hc, baseURL, opts),
		Message:   chainmailv1connect.NewMessageServiceClient(hc, baseURL, opts, connect.WithReadMaxBytes(maxMessageBytes)),
		Compose:   chainmailv1connect.NewComposeServiceClient(hc, baseURL, opts, connect.WithReadMaxBytes(maxComposeBytes)),
		Organize:  chainmailv1connect.NewOrganizeServiceClient(hc, baseURL, opts, connect.WithReadMaxBytes(maxMessageBytes)),
	}
}

// Close drops the client's idle connections to the daemon, so a daemon shutting
// down need not wait for them. The client stays usable and redials on the next call.
func (c *Client) Close() {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// clientHeader names the client kind on every call, unary and streaming,
// for the daemon's audit log.
type clientHeader struct{ kind string }

func (h clientHeader) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set(api.HeaderClient, h.kind)
		return next(ctx, req)
	}
}

func (h clientHeader) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set(api.HeaderClient, h.kind)
		return conn
	}
}

func (h clientHeader) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
