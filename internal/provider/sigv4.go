package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws-samples/sample-llm-gateway/internal/config"
)

// sigV4Auth signs outbound requests for Amazon Bedrock.
//
// Base credentials come from the default chain (on EKS with IRSA: AWS_ROLE_ARN +
// AWS_WEB_IDENTITY_TOKEN_FILE). If role_arn is set, the base identity assumes that role
// first (role chaining, e.g. into a separate Bedrock account) and the assumed credentials
// are cached and refreshed automatically.
//
// All STS traffic (AssumeRoleWithWebIdentity and AssumeRole) is pinned to sts_region so a
// provider that signs for us-west-2 can still reach STS through the local VPC endpoint of
// the region the gateway runs in. The signing region stays the provider's region.
type sigV4Auth struct {
	creds  aws.CredentialsProvider
	signer *v4.Signer
	region string
}

const bedrockServiceName = "bedrock"

func newSigV4(ctx context.Context, code string, pc config.ProviderConfig) (*sigV4Auth, error) {
	stsRegion := pc.STSRegion
	if stsRegion == "" {
		stsRegion = os.Getenv("AWS_REGION")
	}
	if stsRegion == "" {
		stsRegion = os.Getenv("AWS_DEFAULT_REGION")
	}
	if stsRegion == "" {
		stsRegion = pc.Region
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(stsRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	creds := cfg.Credentials
	if pc.RoleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		p := stscreds.NewAssumeRoleProvider(stsClient, pc.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "llm-gateway-" + code
			if pc.ExternalID != "" {
				o.ExternalID = aws.String(pc.ExternalID)
			}
		})
		creds = aws.NewCredentialsCache(p)
	}

	// Fail fast at startup if no credentials can be resolved, and log who we are so
	// cross-account setups can be verified from the gateway log.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := creds.Retrieve(cctx); err != nil {
		return nil, fmt.Errorf("resolve aws credentials: %w", err)
	}
	who := sts.NewFromConfig(cfg, func(o *sts.Options) { o.Credentials = creds })
	if id, err := who.GetCallerIdentity(cctx, &sts.GetCallerIdentityInput{}); err == nil {
		slog.Info("aws identity resolved", "provider", code, "arn", aws.ToString(id.Arn),
			"sign_region", pc.Region, "sts_region", stsRegion, "role_arn", pc.RoleARN)
	} else {
		slog.Warn("aws GetCallerIdentity failed (continuing)", "provider", code, "err", err)
	}
	return &sigV4Auth{creds: creds, signer: v4.NewSigner(), region: pc.Region}, nil
}

func (s *sigV4Auth) Authenticate(ctx context.Context, req *http.Request, body []byte) error {
	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("aws credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	// Clear any client-supplied auth material before signing.
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	return s.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), bedrockServiceName, s.region, time.Now())
}
