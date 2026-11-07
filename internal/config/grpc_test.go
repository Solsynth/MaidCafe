package config

import (
	"os"
	"path/filepath"
	"testing"

	"src.solsynth.dev/sosys/lighthouse/shared/platform/grpctls"
)

// writeFile drops a small file next to a temp config so ClientFiles can read it.
func writeFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cloudConfig(t *testing.T, extra string) *Config {
	t.Helper()
	cfg, err := Load(writeConfig(t, `
[database]
dsn = "host=db dbname=maidcafe"
[discovery]
target = "blade:7001"
service = "stargate"
[workspace]
target = "valve:9090"
`+extra))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestClientFilesReadsGRPCBlock proves the [grpc] keys reach the fleet dials,
// which is what makes Stargate's clientCAFile accept the cloud.
func TestClientFilesReadsGRPCBlock(t *testing.T) {
	dir := t.TempDir()
	ca := writeFile(t, dir, "ca.crt")
	cert := writeFile(t, dir, "client.crt")
	key := writeFile(t, dir, "client.key")
	cfg := cloudConfig(t, `
[grpc]
caFile = "`+ca+`"
clientCertFile = "`+cert+`"
clientKeyFile = "`+key+`"
`)
	if err := cfg.ValidateCloud(); err != nil {
		t.Fatalf("ValidateCloud() error = %v", err)
	}
	files := cfg.ClientFiles()
	if files.CAFile != ca || files.CertFile != cert || files.KeyFile != key {
		t.Fatalf("ClientFiles() = %+v, want the configured files", files)
	}
}

// TestClientFilesFallsBackToEnv keeps the documented env-only deployment
// working: one secret mount, no config edits.
func TestClientFilesFallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	ca := writeFile(t, dir, "ca.crt")
	cert := writeFile(t, dir, "client.crt")
	key := writeFile(t, dir, "client.key")
	t.Setenv("GRPC_CA_FILE", ca)
	t.Setenv("GRPC_CLIENT_CERT_FILE", cert)
	t.Setenv("GRPC_CLIENT_KEY_FILE", key)

	cfg := cloudConfig(t, "")
	files := cfg.ClientFiles()
	if files.CAFile != ca || files.CertFile != cert || files.KeyFile != key {
		t.Fatalf("ClientFiles() = %+v, want the environment files", files)
	}
}

// TestValidateCloudRejectsPartialClientIdentity catches half a certificate
// before the first RPC fails the handshake.
func TestValidateCloudRejectsPartialClientIdentity(t *testing.T) {
	dir := t.TempDir()
	cert := writeFile(t, dir, "client.crt")
	cfg := cloudConfig(t, `
[grpc]
clientCertFile = "`+cert+`"
`)
	if err := cfg.ValidateCloud(); err == nil {
		t.Fatal("ValidateCloud() accepted a client certificate without its key")
	}
}

// TestValidateCloudRejectsMissingCAFile fails fast instead of skipping
// verification when a CA path is wrong.
func TestValidateCloudRejectsMissingCAFile(t *testing.T) {
	cfg := cloudConfig(t, `
[grpc]
caFile = "/nonexistent/ca.crt"
`)
	if err := cfg.ValidateCloud(); err == nil {
		t.Fatal("ValidateCloud() accepted an unreadable CA file")
	}
}

// TestValidateCloudAcceptsEmptyGRPCBlock keeps the pre-mTLS deployment valid:
// no [grpc] block still means self-signed skip-verify.
func TestValidateCloudAcceptsEmptyGRPCBlock(t *testing.T) {
	cfg := cloudConfig(t, "")
	if err := cfg.ValidateCloud(); err != nil {
		t.Fatalf("ValidateCloud() error = %v", err)
	}
	if files := cfg.ClientFiles(); files != (grpctls.Files{}) {
		t.Fatalf("ClientFiles() = %+v, want empty material", files)
	}
}
