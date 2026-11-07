package grpctl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
	dyauth "src.solsynth.dev/sosys/lighthouse/shared/pkg/auth"
	shareddiscovery "src.solsynth.dev/sosys/lighthouse/shared/platform/discovery"
	"src.solsynth.dev/sosys/lighthouse/shared/platform/grpctls"
	gen "src.solsynth.dev/sosys/lighthouse/shared/proto"
)

// fleetCA is a throwaway CA plus the file paths a service would mount.
type fleetCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	caFile  string
	dirPath string
}

func newFleetCA(t *testing.T) *fleetCA {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "maidcafe-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	caFile := filepath.Join(dir, "ca.crt")
	writePEM(t, caFile, "CERTIFICATE", der)
	return &fleetCA{cert: cert, key: key, caFile: caFile, dirPath: dir}
}

// issue writes a leaf certificate for usage, signed by the CA.
func (ca *fleetCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create %s certificate: %v", name, err)
	}
	derKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}
	certFile := filepath.Join(ca.dirPath, name+".crt")
	keyFile := filepath.Join(ca.dirPath, name+".key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", derKey)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startMutualTLSServer serves DyAuthService on a listener that requires a
// client certificate signed by ca, mirroring a fleet service with clientCAFile.
func startMutualTLSServer(t *testing.T, ca *fleetCA) string {
	t.Helper()
	serverCert, serverKey := ca.issue(t, "stargate", x509.ExtKeyUsageServerAuth)
	creds, err := grpctls.Server(serverCert, serverKey, ca.caFile)
	if err != nil {
		t.Fatalf("load server credentials: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(creds))
	gen.RegisterDyAuthServiceServer(server, stubAuth{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

type stubAuth struct {
	gen.UnimplementedDyAuthServiceServer
}

func (stubAuth) Authenticate(context.Context, *gen.DyAuthenticateRequest) (*gen.DyAuthenticateResponse, error) {
	return &gen.DyAuthenticateResponse{Valid: true, Session: &gen.DyAuthSession{
		Account: &gen.DyAccount{Id: "account-a"},
	}}, nil
}

func (stubAuth) HasPermission(context.Context, *gen.DyHasPermissionRequest) (*wrapperspb.BoolValue, error) {
	return wrapperspb.Bool(true), nil
}

// TestDialPresentsClientCertificateToMutualTLSPeer proves the cloud's fleet
// dial satisfies a server that requires a client certificate: the same target
// fails closed without one.
func TestDialPresentsClientCertificateToMutualTLSPeer(t *testing.T) {
	ca := newFleetCA(t)
	target := startMutualTLSServer(t, ca)
	clientCert, clientKey := ca.issue(t, "maidcafe", x509.ExtKeyUsageClientAuth)

	conn, err := Dial(target, true, false, grpctls.Files{
		CAFile:   ca.caFile,
		CertFile: clientCert,
		KeyFile:  clientKey,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := gen.NewDyAuthServiceClient(conn).Authenticate(ctx, &gen.DyAuthenticateRequest{Token: "t"})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !resp.GetValid() || resp.GetSession().GetAccount().GetId() != "account-a" {
		t.Fatalf("Authenticate() = %+v, want the stub answer", resp)
	}

	anonymous, err := Dial(target, true, false, grpctls.Files{CAFile: ca.caFile})
	if err != nil {
		t.Fatalf("Dial() without a client certificate error = %v", err)
	}
	t.Cleanup(func() { _ = anonymous.Close() })
	unauthenticated, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCall()
	if _, err := gen.NewDyAuthServiceClient(anonymous).Authenticate(unauthenticated, &gen.DyAuthenticateRequest{Token: "t"}); err == nil {
		t.Fatal("Authenticate() without a client certificate succeeded; the listener is not enforcing mutual TLS")
	}
}

// TestAuthAuthenticatorPresentsClientCertificate covers the Stargate token
// validation path, which builds its own transport in the shared auth package.
func TestAuthAuthenticatorPresentsClientCertificate(t *testing.T) {
	ca := newFleetCA(t)
	target := startMutualTLSServer(t, ca)
	clientCert, clientKey := ca.issue(t, "maidcafe", x509.ExtKeyUsageClientAuth)

	authenticator, err := dyauth.NewGrpcTokenAuthenticator(dyauth.GrpcAuthDialConfig{
		Target:   target,
		UseTLS:   true,
		CAFile:   ca.caFile,
		CertFile: clientCert,
		KeyFile:  clientKey,
	})
	if err != nil {
		t.Fatalf("NewGrpcTokenAuthenticator() error = %v", err)
	}
	t.Cleanup(func() { _ = authenticator.Close() })

	request, err := http.NewRequest(http.MethodPost, "http://maidcafe.local/api", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer t")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := authenticator.Authenticate(ctx, dyauth.TokenInfo{Token: "t"}, request)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if result.Account.GetId() != "account-a" {
		t.Fatalf("Authenticate() account = %q, want account-a", result.Account.GetId())
	}
}

// TestServerRejectsWrongCA proves the CA bundle is actually verified rather
// than skipped.
func TestServerRejectsWrongCA(t *testing.T) {
	ca := newFleetCA(t)
	other := newFleetCA(t)
	target := startMutualTLSServer(t, ca)
	clientCert, clientKey := other.issue(t, "maidcafe", x509.ExtKeyUsageClientAuth)

	conn, err := Dial(target, true, false, grpctls.Files{
		CAFile:   other.caFile,
		CertFile: clientCert,
		KeyFile:  clientKey,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gen.NewDyAuthServiceClient(conn).Authenticate(ctx, &gen.DyAuthenticateRequest{Token: "t"}); err == nil {
		t.Fatal("Authenticate() succeeded with a certificate from another CA")
	}
}

// TestSkipVerifyOverridesCA keeps the documented escape hatch: tlsSkipVerify
// drops the CA so self-signed deployments still connect.
func TestSkipVerifyOverridesCA(t *testing.T) {
	ca := newFleetCA(t)
	other := newFleetCA(t)
	target := startMutualTLSServer(t, ca)
	clientCert, clientKey := ca.issue(t, "maidcafe", x509.ExtKeyUsageClientAuth)

	conn, err := Dial(target, true, true, grpctls.Files{
		CAFile:   other.caFile,
		CertFile: clientCert,
		KeyFile:  clientKey,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gen.NewDyAuthServiceClient(conn).Authenticate(ctx, &gen.DyAuthenticateRequest{Token: "t"}); err != nil {
		t.Fatalf("Authenticate() with tlsSkipVerify error = %v", err)
	}
}

// TestCredentialsRequireBothFiles rejects half a client identity.
func TestCredentialsRequireBothFiles(t *testing.T) {
	if _, err := Credentials(true, false, grpctls.Files{CertFile: "client.crt"}); err == nil {
		t.Fatal("Credentials() accepted a certificate without its key")
	}
}

// TestDiscoveryResolvesMutualTLSPeers runs the production wiring: Blade's
// registry is dialed with the client certificate, and the resolved Stargate
// endpoint is reached through blade:/// with TLS verification.
func TestDiscoveryResolvesMutualTLSPeers(t *testing.T) {
	ca := newFleetCA(t)
	clientCert, clientKey := ca.issue(t, "maidcafe", x509.ExtKeyUsageClientAuth)
	files := grpctls.Files{CAFile: ca.caFile, CertFile: clientCert, KeyFile: clientKey}

	stargateTarget := startMutualTLSServer(t, ca)
	bladeTarget := startMutualTLSRegistry(t, ca, stargateTarget)

	provider, err := shareddiscovery.NewRemoteProvider(bladeTarget, files)
	if err != nil {
		t.Fatalf("NewRemoteProvider() error = %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	resolver := shareddiscovery.NewResolver(provider, nil)
	shareddiscovery.RegisterResolver(resolver)

	conn, err := Dial(shareddiscovery.Target("stargate"), true, false, files)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gen.NewDyAuthServiceClient(conn).Authenticate(ctx, &gen.DyAuthenticateRequest{Token: "t"}); err != nil {
		t.Fatalf("Authenticate() through discovery error = %v", err)
	}
}

// startMutualTLSRegistry serves DyServiceDiscoveryService over mTLS and
// resolves "stargate" to endpoint, mirroring Blade's internal gRPC listener.
func startMutualTLSRegistry(t *testing.T, ca *fleetCA, endpoint string) string {
	t.Helper()
	serverCert, serverKey := ca.issue(t, "blade", x509.ExtKeyUsageServerAuth)
	creds, err := grpctls.Server(serverCert, serverKey, ca.caFile)
	if err != nil {
		t.Fatalf("load registry credentials: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(creds))
	gen.RegisterDyServiceDiscoveryServiceServer(server, registry{endpoint: endpoint})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

type registry struct {
	gen.UnimplementedDyServiceDiscoveryServiceServer
	endpoint string
}

func (r registry) Resolve(_ context.Context, req *gen.DyResolveServiceRequest) (*gen.DyResolveServiceResponse, error) {
	if req.GetService() != "stargate" {
		return &gen.DyResolveServiceResponse{}, nil
	}
	return &gen.DyResolveServiceResponse{Instances: []*gen.DyServiceInstance{{
		Service:      "stargate",
		InstanceId:   "stargate-1",
		Healthy:      true,
		GrpcEndpoint: r.endpoint,
	}}}, nil
}
