// Package grpctl builds the cloud's outbound gRPC transports. Every fleet
// dependency (Blade discovery, Stargate, Valve, Metoer) is dialed through it so
// one client certificate and one CA bundle cover the whole surface, including
// peers that require mutual TLS.
package grpctl

import (
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	dyauth "src.solsynth.dev/sosys/lighthouse/shared/pkg/auth"
	"src.solsynth.dev/sosys/lighthouse/shared/platform/grpctls"
)

// Credentials returns the transport for one fleet dial. A target without TLS
// keeps plaintext credentials (Blade's h2c listener); a TLS target presents
// files to peers enforcing mutual TLS and verifies them when files.CAFile is
// set. skipVerify is an explicit override: it drops the CA so self-signed
// deployments keep working, and an empty CA skips verification anyway.
func Credentials(useTLS, skipVerify bool, files grpctls.Files) (credentials.TransportCredentials, error) {
	if !useTLS {
		return insecure.NewCredentials(), nil
	}
	if skipVerify {
		files.CAFile = ""
	}
	creds, err := grpctls.Client(files)
	if err != nil {
		return nil, fmt.Errorf("load gRPC client credentials: %w", err)
	}
	return creds, nil
}

// Dial connects to a fleet target. The scheme selects the transport: grpc://
// and http:// are plaintext, grpcs:// and https:// are TLS, and a bare
// host:port falls back to useTLS.
func Dial(target string, useTLS, skipVerify bool, files grpctls.Files) (*grpc.ClientConn, error) {
	address, tls := dyauth.NormalizeAuthGRPCTarget(strings.TrimSpace(target), useTLS)
	if address == "" {
		return nil, fmt.Errorf("gRPC target is empty")
	}
	creds, err := Credentials(tls, skipVerify, files)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	return conn, nil
}
