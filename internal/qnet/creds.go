package qnet

import (
	"context"
	"errors"
	"net"

	"github.com/candyflipline/mnemonic/internal/auth"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type authInfo struct {
	credentials.CommonAuthInfo
	id auth.Identity
}

func (authInfo) AuthType() string { return "qnet" }

type creds struct{}

func Credentials() credentials.TransportCredentials { return creds{} }

func FromPeer(ctx context.Context) (auth.Identity, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return auth.Identity{}, false
	}
	ai, ok := p.AuthInfo.(authInfo)
	return ai.id, ok
}

func (creds) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	id, ok := IdentityOf(c)
	if !ok {
		return nil, nil, errors.New("qnet: not a conn accepted from a qnet listener")
	}
	return c, authInfo{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}, id: id}, nil
}

func (creds) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("qnet: credentials are server side only; dial with qnet.Dial and insecure credentials")
}

func (creds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "qnet"}
}

func (creds) Clone() credentials.TransportCredentials { return creds{} }

func (creds) OverrideServerName(string) error { return nil }
