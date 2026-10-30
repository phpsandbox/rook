package update

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

const githubIssuer = "https://token.actions.githubusercontent.com"
const provenanceType = "https://slsa.dev/provenance/v1"

func releaseIdentity(tag string) (verify.CertificateIdentity, error) {
	if _, ok := parseVersion(tag); !ok {
		return verify.CertificateIdentity{}, errors.New("invalid release verification tag")
	}
	identity, err := verify.NewShortCertificateIdentity(githubIssuer, "", "https://github.com/"+repository+"/.github/workflows/release.yml@refs/tags/"+tag, "")
	if err != nil {
		return identity, err
	}
	// These fields come from the signed certificate, not workflow-controlled
	// provenance payload. They bind the artifact to this tagged, hosted build.
	identity.Extensions.SourceRepositoryURI = "https://github.com/" + repository
	identity.Extensions.SourceRepositoryRef = "refs/tags/" + tag
	identity.Extensions.RunnerEnvironment = "github-hosted"
	return identity, nil
}

type trustHTTPClient struct {
	ctx    context.Context
	client *http.Client
}

func (c trustHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(request.WithContext(c.ctx))
}

func releaseTrustedRoot(ctx context.Context, cachePath string) (*root.TrustedRoot, error) {
	options := tuf.DefaultOptions().WithCachePath(cachePath).WithContext(ctx)
	// The SDK embeds the initial Sigstore TUF root and verifies signed metadata,
	// expiry and key rotations. Never take trust roots from a release bundle.
	metadataFetcher := fetcher.NewDefaultFetcher()
	metadataFetcher.SetRetry(0, 1)
	metadataFetcher.SetHTTPUserAgent("rook-auto-update")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected trust metadata redirect") }}
	defer client.CloseIdleConnections()
	metadataFetcher.SetHTTPClient(trustHTTPClient{ctx: ctx, client: client})
	options.Fetcher = metadataFetcher
	return root.FetchTrustedRootWithOptions(options)
}

func verifySignedProvenance(entity verify.SignedEntity, trusted root.TrustedMaterial, algorithm string, digest []byte, identity verify.CertificateIdentity) error {
	verifier, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1), verify.WithSignedCertificateTimestamps(1))
	if err != nil {
		return err
	}
	result, err := verifier.Verify(entity, verify.NewPolicy(verify.WithArtifactDigest(algorithm, digest), verify.WithCertificateIdentity(identity)))
	if err != nil {
		return fmt.Errorf("verify signed build attestation: %w", err)
	}
	if result.Statement == nil || result.Statement.PredicateType != provenanceType {
		return errors.New("release attestation must contain SLSA build provenance")
	}
	return nil
}

func verifyAttestation(ctx context.Context, binaryPath, bundlePath, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	identity, err := releaseIdentity(tag)
	if err != nil {
		return err
	}
	signedBundle, err := bundle.LoadJSONFromPath(bundlePath)
	if err != nil {
		return fmt.Errorf("read signed build attestation: %w", err)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return err
	}
	trusted, err := releaseTrustedRoot(ctx, filepath.Join(filepath.Dir(bundlePath), "trust"))
	if err != nil {
		return fmt.Errorf("load release verification trust: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digest := sha256.Sum256(binary)
	return verifySignedProvenance(signedBundle, trusted, "sha256", digest[:], identity)
}
