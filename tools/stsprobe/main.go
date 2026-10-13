// Command stsprobe reproduces, from inside a pod, how an SDK-based gateway obtains cross-account
// credentials and calls Bedrock, so the two STS hops can be observed one at a time:
//
//  1. IRSA: the SDK's default credential chain calls STS AssumeRoleWithWebIdentity using a client
//     the SDK builds internally (the caller never sees it).
//  2. Cross-account: the program builds its own STS client and calls AssumeRole into the Bedrock
//     account.
//
// Flags select where each hop is pointed, covering the configurations compared in
// docs/cross-account-private-networking.md §3:
//
//	-sts-region      region used for BOTH hops unless overridden (the gateway's sts_region idea)
//	-sts-base        BaseEndpoint set on the SELF-BUILT client only (hop 2); hop 1 is unaffected
//	AWS_ENDPOINT_URL_STS (env)  applies to every STS client in the process, hop 1 included
//
// Every HTTP request the SDK makes is logged with its host, so the output shows exactly which
// hostname each hop resolved and whether it reached a VPC endpoint or timed out on the public one.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type logRT struct{ next http.RoundTripper }

func (l logRT) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := l.next.RoundTrip(r)
	if err != nil {
		fmt.Printf("  http  %-6s %-75s ERR after %s: %s\n", r.Method, r.URL.Host+r.URL.Path, time.Since(start).Round(time.Millisecond), short(err.Error()))
		return nil, err
	}
	fmt.Printf("  http  %-6s %-75s HTTP %d in %s\n", r.Method, r.URL.Host+r.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	return resp, nil
}

func short(s string) string {
	if i := strings.LastIndex(s, ": "); i > 0 {
		return s[i+2:]
	}
	return s
}

func main() {
	stsRegion := flag.String("sts-region", "us-west-2", "region for LoadDefaultConfig (drives hop 1 and hop 2 unless -sts-base is set); \"\" = do not set, let the SDK read AWS_REGION / AWS_DEFAULT_REGION")
	stsBase := flag.String("sts-base", "", "BaseEndpoint for the self-built STS client only (hop 2)")
	roleARN := flag.String("role", "", "cross-account role to assume (hop 2)")
	brRegion := flag.String("bedrock-region", "us-west-2", "Bedrock signing region")
	brBase := flag.String("bedrock-base", "", "Bedrock endpoint (VPCE DNS name for cross-region peering)")
	model := flag.String("model", "us.anthropic.claude-sonnet-5", "model id")
	timeout := flag.Duration("timeout", 8*time.Second, "HTTP timeout per attempt")
	flag.Parse()

	fmt.Printf("stsprobe: sts-region=%q sts-base=%q AWS_ENDPOINT_URL_STS=%q AWS_REGION=%q AWS_DEFAULT_REGION=%q role=%s\n",
		*stsRegion, *stsBase, os.Getenv("AWS_ENDPOINT_URL_STS"), os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), *roleARN)
	hc := &http.Client{Timeout: *timeout, Transport: logRT{http.DefaultTransport}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Hop 1 lives inside here: with AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE set by IRSA, the
	// default chain creates its own STS client and calls AssumeRoleWithWebIdentity lazily.
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithHTTPClient(hc),
		awsconfig.WithRetryer(func() aws.Retryer { return aws.NopRetryer{} })}
	if *stsRegion != "" {
		opts = append(opts, awsconfig.WithRegion(*stsRegion))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		fail("LoadDefaultConfig", err)
	}
	fmt.Printf("  effective SDK region: %q\n", cfg.Region)

	// Hop 2: the program's own STS client. -sts-base sets BaseEndpoint on this client only; it does
	// not reach the client the SDK built for hop 1.
	stsClient := sts.NewFromConfig(cfg, func(o *sts.Options) {
		if *stsBase != "" {
			o.BaseEndpoint = aws.String(*stsBase)
		}
	})
	var creds aws.CredentialsProvider = cfg.Credentials
	if *roleARN != "" {
		creds = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, *roleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = "stsprobe"
		}))
	}
	fmt.Println("step 1+2: resolve credentials (IRSA AssumeRoleWithWebIdentity, then AssumeRole)")
	cv, err := creds.Retrieve(ctx)
	if err != nil {
		fail("credentials", err)
	}
	who, err := sts.NewFromConfig(cfg, func(o *sts.Options) {
		o.Credentials = creds
		if *stsBase != "" {
			o.BaseEndpoint = aws.String(*stsBase)
		}
	}).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		fail("GetCallerIdentity", err)
	}
	fmt.Printf("  identity: %s (source=%s)\n", aws.ToString(who.Arn), cv.Source)

	fmt.Println("step 3: Bedrock InvokeModel with the assumed credentials")
	br := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		o.Region = *brRegion
		o.Credentials = creds
		if *brBase != "" {
			o.BaseEndpoint = aws.String(*brBase)
		}
	})
	body := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":20,"messages":[{"role":"user","content":"Say hi in 3 words."}]}`
	if strings.Contains(*model, "openai") {
		body = `{"messages":[{"role":"user","content":"Say hi in 3 words."}],"max_completion_tokens":20,"reasoning_effort":"none"}`
	}
	out, err := br.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{ModelId: model, Body: []byte(body),
		ContentType: aws.String("application/json"), Accept: aws.String("application/json")})
	if err != nil {
		fail("InvokeModel", err)
	}
	fmt.Printf("  response: %.160s\nRESULT: OK\n", out.Body)
}

func fail(step string, err error) {
	fmt.Printf("RESULT: FAILED at %s\n  %v\n", step, err)
	os.Exit(1)
}
