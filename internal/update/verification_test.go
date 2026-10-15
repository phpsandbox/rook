package update

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Public Sigstore fixtures copied unchanged from sigstore-go v1.3.0,
// pkg/testing/data; their Apache 2.0 license is retained in testdata/LICENSE.
// They exercise real signatures, certificates, timestamps and log proofs.
func TestNativeVerificationChecksSignatureDigestAndIdentity(t *testing.T) {
	signed, err := bundle.LoadJSONFromPath("testdata/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := root.NewTrustedRootFromPath("testdata/trusted-root.json")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verify.NewShortCertificateIdentity(githubIssuer, "", "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main", "")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString("46d4e2f74c4877316640000a6fdf8a8b59f1e0847667973e9859f774dd31b8f1e0937813b777fb66a2ac67d50540fe34640966eee9fc2ccca387082b4c85cd3c")
	if err != nil {
		t.Fatal(err)
	}
	// Verification works with no command-line tools available.
	t.Setenv("PATH", t.TempDir())
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, identity); err != nil {
		t.Fatal(err)
	}
	rookIdentity, err := releaseIdentity("v0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, rookIdentity); err == nil {
		t.Fatal("accepted attestation from another repository/workflow")
	}
	digest[0] ^= 1
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, identity); err == nil {
		t.Fatal("accepted wrong artifact digest")
	}
	digest[0] ^= 1
	signed.Bundle.GetDsseEnvelope().Signatures[0].Sig[0] ^= 1
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, identity); err == nil {
		t.Fatal("accepted altered signature")
	}
	signed, err = bundle.LoadJSONFromPath("testdata/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	// Changing the statement invalidates its signature even if the original
	// artifact digest and signing certificate still match.
	signed.Bundle.GetDsseEnvelope().Payload = []byte("tampered")
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, identity); err == nil {
		t.Fatal("accepted altered payload")
	}
}

func TestReleasePolicyPinsTagIssuerSourceAndHostedRunner(t *testing.T) {
	identity, err := releaseIdentity("v0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	valid := certificate.Summary{
		SubjectAlternativeName: "https://github.com/phpsandbox/rook/.github/workflows/release.yml@refs/tags/v0.2.2",
		Extensions:             certificate.Extensions{Issuer: githubIssuer, SourceRepositoryURI: "https://github.com/phpsandbox/rook", SourceRepositoryRef: "refs/tags/v0.2.2", RunnerEnvironment: "github-hosted"},
	}
	if err := identity.Verify(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*certificate.Summary){
		func(c *certificate.Summary) {
			c.SubjectAlternativeName = "https://github.com/phpsandbox/rook/.github/workflows/other.yml@refs/tags/v0.2.2"
		},
		func(c *certificate.Summary) {
			c.SubjectAlternativeName = "https://github.com/phpsandbox/rook/.github/workflows/release.yml@refs/tags/v0.2.1"
		},
		func(c *certificate.Summary) { c.Issuer = "https://untrusted.example" },
		func(c *certificate.Summary) { c.SourceRepositoryURI = "https://github.com/attacker/rook" },
		func(c *certificate.Summary) { c.SourceRepositoryRef = "refs/heads/main" },
		func(c *certificate.Summary) { c.RunnerEnvironment = "self-hosted" },
		func(c *certificate.Summary) { c.RunnerEnvironment = "" },
	} {
		invalid := valid
		mutate(&invalid)
		if err := identity.Verify(invalid); err == nil {
			t.Fatalf("accepted certificate %+v", invalid)
		}
	}
	if _, err := releaseIdentity("v0.2.2/other"); err == nil {
		t.Fatal("accepted invalid verification tag")
	}
}

func TestMalformedBundleFailsWithoutExternalTools(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "invalid.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	if err := verifyAttestation(context.Background(), "missing-binary", path, "v0.2.2"); err == nil {
		t.Fatal("accepted unsigned bundle")
	}
}

// Optional integration check for the public TUF repository, kept out of normal
// tests so they require neither Internet access nor external verification tools.
func TestLiveTrustRootRefresh(t *testing.T) {
	if os.Getenv("ROOK_TEST_LIVE_TRUST") != "1" {
		t.Skip("set ROOK_TEST_LIVE_TRUST=1 to check public trust metadata")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	trusted, err := releaseTrustedRoot(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := bundle.LoadJSONFromPath("testdata/provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verify.NewShortCertificateIdentity(githubIssuer, "", "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main", "")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString("46d4e2f74c4877316640000a6fdf8a8b59f1e0847667973e9859f774dd31b8f1e0937813b777fb66a2ac67d50540fe34640966eee9fc2ccca387082b4c85cd3c")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySignedProvenance(signed, trusted, "sha512", digest, identity); err != nil {
		t.Fatal(err)
	}
}
