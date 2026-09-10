# Sample LLM Gateway

A lightweight, protocol-preserving large language model (LLM) gateway written in Go. This sample shows how to connect OpenAI- and Anthropic-compatible clients to Amazon Bedrock or other compatible HTTP providers while keeping authentication, quotas, and billing in an existing control plane.

The gateway implements the request-forwarding layer, not a complete model management platform. It integrates with your control plane through three HTTP endpoints for model routes, authorization, and usage reporting. A mock control plane is included for development and testing.

> **Sample code:** Use a development or test environment. This project is not a production-ready service. Review the [security considerations](#security-considerations), [known limitations](#known-limitations), and [costs](#costs) before deploying it.

## Contents

- [Architecture and scope](#architecture-and-scope)
- [Prerequisites](#prerequisites)
- [Getting started](#getting-started)
- [Control plane integration](#control-plane-integration)
- [Request and response handling](#request-and-response-handling)
- [Deployment on Amazon EKS](#deployment-on-amazon-eks)
- [Observability](#observability)
- [Testing](#testing)
- [Security considerations](#security-considerations)
- [Known limitations](#known-limitations)
- [Costs](#costs)
- [Clean up](#clean-up)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Architecture and scope

```text
Clients
  POST /v1/chat/completions ---+
  POST /v1/responses ---------+--> LLM gateway --> Amazon Bedrock (SigV4)
  POST /v1/messages ----------+        |       --> Compatible HTTP providers
                                      |
                                      +--> Your control plane
                                           - Model routes
                                           - Authorization and quotas
                                           - Usage reporting
```

The gateway preserves the incoming protocol: Chat Completions requests go to a Chat Completions endpoint, Responses requests to a Responses endpoint, and Messages requests to a Messages endpoint. It does not translate between these APIs or call the Amazon Bedrock Converse API.

The gateway provides:

- API key authorization through your control plane, including its decisions on model access, rate limits, and quotas.
- Dynamic route updates, priority-based routing, and weighted candidate selection within each priority tier.
- Provider authentication using `aws_iam`, `bearer`, `x-api-key`, or `none`.
- Streaming response relay and failover for eligible errors before committing to a response.
- Asynchronous token usage reporting with a bounded in-memory queue and retries.
- Health checks, Prometheus metrics, structured JSON logs, and a model listing endpoint.

For Amazon Bedrock, the gateway signs requests with AWS Signature Version 4 (SigV4) using the AWS SDK default credential chain. Optional AWS Security Token Service (AWS STS) role assumption supports cross-account access. Configurable endpoints support cross-Region and private connectivity when the required IAM and networking resources are in place.

The gateway does not issue API keys, calculate prices, deduct quotas, persist usage records, or provide a management UI. It does not create a public ingress endpoint. These responsibilities remain with your control plane and deployment infrastructure.

## Prerequisites

For local development:

- Go 1.26 or later, as specified in [go.mod](go.mod).
- Git and `curl`.
- Python 3.12 or later for the smoke test script. The usage display below also uses Python.

For the Amazon Bedrock quick start:

- An AWS account and credentials available through the [AWS SDK default credential chain](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html). Use temporary credentials, such as an IAM Identity Center session.
- Access to a model that supports the Anthropic Messages API on your selected Amazon Bedrock Runtime endpoint, with permission to invoke it. Check the [Amazon Bedrock documentation](https://docs.aws.amazon.com/bedrock/latest/userguide/models-supported.html) for model, API, and AWS Region availability.
- The exact model or inference profile ID accepted by that endpoint. Use a supported cross-Region inference profile where appropriate; do not add `global.` unless the model and endpoint support it.

You do not need an Amazon EKS cluster for local testing. AWS credentials are not required when using only non-AWS providers with environment-based API keys and no Secrets Manager references.

## Getting started

This example runs the gateway and mock control plane locally, then sends a real inference request to Amazon Bedrock. Inference charges apply. The mock does not emulate a model or provide production authorization and billing.

### 1. Clone and build

```bash
git clone https://github.com/aws-samples/sample-llm-gateway.git
cd sample-llm-gateway
go build -o bin/gateway ./cmd/gateway
go build -o bin/mock-controlplane ./cmd/mock-controlplane
```

Run the remaining commands from the repository root. Keep the mock control plane and gateway running in separate terminals.

### 2. Create a local configuration and route

Create `configs/gateway.local.yaml` with the following content. This filename is ignored by Git. The gateway binds only to the loopback interface and enables only the Bedrock provider, so no third-party API keys or Secrets Manager secrets are needed.

```yaml
server:
  listen: "127.0.0.1:8080"
control_plane:
  base_url: "http://127.0.0.1:9090"
  allow_insecure: true # Local mock only; use HTTPS for a real control plane.
  token: "mock-token"
providers:
  bedrock:
    auth: aws_iam
    region: "${AWS_REGION}"
    endpoints:
      anthropic: "https://bedrock-runtime.${AWS_REGION}.amazonaws.com/anthropic/v1"
```

Create a local working directory:

```bash
mkdir -p .local
```

Save the following as `.local/routes.json`, which is also ignored by Git. Replace `YOUR_MESSAGES_MODEL_OR_INFERENCE_PROFILE_ID` with the exact ID for your chosen Messages-compatible model. `demo-model` is a client-facing alias, not an Amazon Bedrock model ID.

```json
{
  "models": [
    {
      "modelCode": "demo-model",
      "providers": [
        {
          "providerCode": "bedrock",
          "providerModelCode": "YOUR_MESSAGES_MODEL_OR_INFERENCE_PROFILE_ID",
          "priority": 10,
          "weight": 100
        }
      ]
    }
  ]
}
```

### 3. Start the mock control plane

In the first terminal:

```bash
./bin/mock-controlplane \
  -listen 127.0.0.1:9090 \
  -token mock-token \
  -keys sk-demo-key \
  -routes .local/routes.json
```

These are public test credentials. Do not reuse them outside this local example. The mock's `/debug/*` endpoints are unauthenticated.

### 4. Start the gateway

In the second terminal, configure your AWS credentials and select a Region that supports your chosen model and API. The following uses `ap-northeast-1` as an example; change it if needed.

```bash
export AWS_REGION=ap-northeast-1
./bin/gateway -config configs/gateway.local.yaml
```

The gateway loads the initial route snapshot before accepting requests. A successful startup includes a `gateway listening` log entry.

### 5. Send a request and check usage

In a third terminal:

```bash
curl --fail-with-body -sS http://127.0.0.1:8080/readyz
curl --fail-with-body -sS http://127.0.0.1:8080/v1/models

curl --fail-with-body -sS http://127.0.0.1:8080/v1/messages \
  -H 'Authorization: Bearer sk-demo-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-model","max_tokens":64,"messages":[{"role":"user","content":"Say hello in one sentence."}]}'

curl --fail-with-body -sS http://127.0.0.1:9090/debug/usages | python3 -m json.tool
```

Expect `ready` from `/readyz`, the `demo-model` alias in `/v1/models`, and a Messages response from inference. Usage reporting is asynchronous; repeat the final command if the record has not arrived. A successful inference report should include `status_code: 200` and token counts. Readiness and model listing alone do not verify model access.

For streaming, add `"stream": true` to the request body and use `curl -N`. For another protocol, add its endpoint to the provider configuration and use a model that supports it.

For non-AWS providers, see [configs/local-external.example.yaml](configs/local-external.example.yaml), [configs/routes.external.example.json](configs/routes.external.example.json), and the walkthrough in [docs/operations.md](docs/operations.md). The full [gateway configuration example](configs/gateway.example.yaml) includes optional providers and Secrets Manager references; remove unused providers before using it locally.

## Control plane integration

Implement these endpoints in your control plane. Requests include a shared token in `X-HIGRESS-Token` by default; the header name is configurable.

| Endpoint | Responsibility |
| --- | --- |
| `GET /admin/gateway/model-routes` | Return model aliases and provider candidates. Support ETags for route polling. |
| `POST /admin/gateway/key-auth` | Validate the client key, model access, rate limits, and available quota in one call. |
| `POST /admin/gateway/usage/report` | Accept normalized usage records and deduplicate retries by `request_id`. |

For each inference request, the gateway extracts the key from `Authorization: Bearer <key>` or `x-api-key`, calls `key-auth`, selects a route, and forwards the request. An unavailable or timed-out authorization service results in HTTP 503. Rejections map to HTTP 401, 403, 404, or 429 in the incoming protocol's error format.

Route polling defaults to 30 seconds. Add models by updating control-plane routes; no gateway restart is needed. Adding a provider, changing its configuration, or rotating a configured secret requires a gateway restart. Keep route `providerCode` values aligned with the configured providers.

See [docs/control-plane.md](docs/control-plane.md) for the complete contract, response envelopes, rejection mappings, and token accounting definitions.

## Request and response handling

Protocol preservation does not mean byte-for-byte forwarding of the entire HTTP request. The gateway makes these changes:

| Area | Behavior |
| --- | --- |
| Request model | Replace `model` with `providerModelCode`, without adding prefixes or modifying the ID. |
| Streaming Chat Completions | Set `stream_options.include_usage` to `true`, retaining other stream options. |
| Request headers | Forward only `Accept`, `anthropic-version`, `anthropic-beta`, and `openai-beta` from the client. Set the JSON content type and gateway user agent, and apply provider credentials. |
| Anthropic version | Supply `anthropic-version: 2023-06-01` if absent. |
| Response headers | Forward `Content-Type` and `Cache-Control`; add `X-Request-Id` and, when available, `X-Upstream-Request-Id`. Set `Content-Length` for buffered responses and `X-Accel-Buffering: no` for SSE. |

Other request fields are retained without protocol conversion, although JSON serialization can change whitespace and field ordering. Response bodies are relayed without format conversion, subject to size and error handling limits. Clients must supply parameters supported by the model; the gateway does not rename fields such as `max_tokens` to `max_completion_tokens`.

Before committing to a response, the gateway can try another candidate on local credential/signing errors, transport errors, or HTTP 429/5xx responses. Attempts are bounded by `max_failover_attempts` (default: 3) and the overall request timeout. Other upstream HTTP errors, including 401 and 403, are passed through. The last candidate's HTTP 429/5xx response can also be passed through; failures without a committed upstream response generally produce HTTP 502 or 504. There is no failover after response relay begins.

## Deployment on Amazon EKS

The repository includes an `eksctl` configuration and Kubernetes manifests, not a one-command deployment. The sample uses an existing VPC, EKS Auto Mode, private-subnet nodes, IAM roles for service accounts (IRSA), and a `ClusterIP` Service.

Before applying the manifests:

1. Install and configure the AWS CLI, `eksctl`, `kubectl`, and [ko](https://ko.build). Confirm the target account and Region. Adapt [deploy/eks/cluster.yaml](deploy/eks/cluster.yaml), including the Kubernetes version, VPC, subnets, and restricted API-server access CIDR.
2. Scope the [Bedrock policy](deploy/eks/bedrock-invoke-policy.json) to your models and inference profiles. Configure IRSA and update [the service account](deploy/k8s/10-serviceaccount.yaml). If using Secrets Manager, create the referenced secrets and scope the [secrets-read policy](deploy/eks/secrets-read-policy.json). Remove cross-account configuration unless needed.
3. Build and publish images to your Amazon Elastic Container Registry (Amazon ECR) repositories. Replace the account, Region, and tag below. Update both the image tag and digest in the deployment manifests with your build output.
4. Review [deploy/k8s/05-network.yaml](deploy/k8s/05-network.yaml) separately: it includes cluster-scoped node resources and a `kube-system` configuration change. Both deployments select nodes labeled `network-tier: private`; provide those nodes or adapt the selectors. The private-only egress policy does not allow public third-party API endpoints.
5. Create the namespace, configure the service account and secrets, and apply the reviewed workload manifests. The mock requires a `llm-gateway-secrets` Kubernetes Secret with `CP_GATEWAY_TOKEN` and `MOCK_API_KEYS`. For a real control plane, omit the mock and configure the gateway to use your HTTPS service.
6. Verify rollout status, readiness, a real inference request, and its usage report. Configure TLS and access controls at your chosen ingress if clients need access outside the cluster.

Example image build, after preparing ECR access:

```bash
export KO_DOCKER_REPO="123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/sample-llm-gateway"
ko build --base-import-paths --platform=linux/amd64,linux/arm64 \
  --tags=v0.6.1 ./cmd/gateway ./cmd/mock-controlplane
```

`ko` does not require a Docker daemon. A [Dockerfile](deploy/Dockerfile) is also available for the gateway. The gateway manifests use two replicas, required node anti-affinity, health probes, a PodDisruptionBudget, non-root containers, and read-only root filesystems. These settings support availability and isolation but do not guarantee uninterrupted service.

Account IDs, infrastructure IDs, endpoint hostnames, and image references in deployment examples must be reviewed and replaced. See [docs/private-networking.md](docs/private-networking.md) for the detailed deployment and private connectivity walkthrough.

## Observability

| Endpoint or output | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check. |
| `GET /readyz` | Readiness after initial route loading; returns 503 while draining. Not an upstream availability check. |
| `GET /metrics` | Prometheus metrics prefixed with `llmgw_`. |
| `GET /v1/models` | OpenAI-format listing of the current route snapshot. Not filtered by client permissions. |
| Standard output | JSON logs with request IDs, provider, status, latency, and usage fields. |

Use `X-Request-Id` to correlate inference requests with logs and usage reports. Health, metrics, and model listing endpoints are unauthenticated; restrict access at the network or ingress layer. See [docs/operations.md](docs/operations.md) for metric definitions and troubleshooting.

## Testing

Run local checks without AWS credentials or live model calls:

```bash
go vet ./...
go test -race ./... -count=1
```

The race detector requires a supported platform and a C toolchain. Tests cover protocol parsing, routing, request rewriting, streaming usage, authorization failures, failover, queue behavior, and shutdown scenarios with mock HTTP services.

For a live smoke test against the local quick start:

```bash
GW=http://127.0.0.1:8080 CP=http://127.0.0.1:9090 KEY=sk-demo-key \
  CLAUDE_MODELS="demo-model" GPT_MODELS="" RESPONSES_MODELS="" \
  bash scripts/smoke.sh
```

The script sends real inference requests, tests streaming and non-streaming behavior and negative cases, and checks received usage records. It clears the mock's previous usage records before running. Use only a test control plane and test keys. For other routes, set the three model variables to the appropriate space-separated aliases; an empty value skips that protocol group. Live tests incur provider charges.

[GitHub Actions](.github/workflows/ci.yml) runs `go vet`, race-enabled tests, `govulncheck`, and binary builds. These checks do not validate deployed AWS permissions, networking, or model availability. Load-test scripts and environment-specific results are in [loadtest/README.md](loadtest/README.md); those results are not performance guarantees.

## Security considerations

- **Protect network traffic.** The gateway serves HTTP and does not terminate TLS. Use a secured TLS ingress or proxy for remote clients. Use HTTPS for a real control plane: authorization and usage requests carry client keys and the shared token. `allow_insecure: true` is a test-only exception.
- **Keep the mock private.** Its fixed credentials and unauthenticated debug endpoints are for testing, not production access control.
- **Use least-privilege AWS access.** Prefer temporary credentials and review permissions for the model, inference profile, API, and any cross-account role. Retrieve and compare live IAM policies before changing them; do not overwrite them blindly with sample files.
- **Keep secrets out of source control.** Use environment variables or `secretsmanager://<secret-id>[#<json-key>]` references. Configured secrets are resolved at startup; restart after rotation. Restrict access to provider configuration and route administration.
- **Review data handling.** Prompts and responses pass through to the selected provider. Logs can contain subject identifiers and upstream error snippets; usage reports contain client API keys. Restrict access, define retention, and assess provider terms and data residency requirements before handling sensitive data.

## Known limitations

- Only the three inference APIs listed above are implemented. This is not a full OpenAI or Anthropic API replacement, and it does not translate to Converse or other protocols.
- Authorization is a synchronous control-plane dependency. If `key-auth` is unavailable, inference requests fail closed with HTTP 503.
- Usage reporting is not a durable billing ledger. Records can be lost on a crash, queue overflow, exhausted retries, or an unsuccessful shutdown flush. Reconcile independently with provider usage.
- After response relay begins, the gateway does not retry or inject its own SSE error events. Clients should check completion markers such as `message_stop`, `[DONE]`, or `response.completed` to detect incomplete streams.
- Detected interrupted transfers report zero tokens, with status 499 for client disconnects, 504 for request timeouts, or 502 for upstream transfer errors. These are metering statuses when response headers have already been sent. Providers may still charge for tokens generated before interruption.
- Provider configuration and configured secret changes require a restart; route updates do not. Unconfigured providers or providers missing the required protocol endpoint are skipped and can result in HTTP 502 if no candidate is usable.
- Request bodies are buffered, with a default 32 MiB limit. Non-streaming upstream responses are buffered up to 64 MiB; larger responses are rejected with HTTP 502. Size capacity for concurrent large requests accordingly.
- Model availability, API support, quotas, and geographic eligibility depend on the provider and endpoint. The gateway does not remove these restrictions.

## Costs

You are responsible for charges incurred while using this sample. Local execution creates no AWS infrastructure, but real inference requests are billed by Amazon Bedrock or your chosen provider.

An EKS deployment can also incur charges for the cluster, EKS Auto Mode and compute, Amazon EBS storage, ECR storage, Secrets Manager, VPC endpoints, NAT gateways, data transfer, and any load balancers or logging services you add. Idle infrastructure can continue to incur charges.

Estimate your configuration with the [AWS Pricing Calculator](https://calculator.aws/) and check current [Amazon Bedrock pricing](https://aws.amazon.com/bedrock/pricing/) and [Amazon EKS pricing](https://aws.amazon.com/eks/pricing/).

## Clean up

For the local quick start, stop the gateway with `Ctrl+C`, allow it to finish shutting down, and then stop the mock control plane. These steps create no AWS infrastructure. Remove local binaries, configuration, and the route file if no longer needed.

For an EKS deployment, verify the AWS account, Kubernetes context, and resource ownership before removing anything:

1. Stop test clients and load-test Jobs. Remove sample-specific ingress or load balancer resources while their controllers are still running.
2. Remove the sample workloads, Services, ConfigMaps, Secrets, service account, and network policies in `llm-gateway`. Delete the namespace only if dedicated to this sample.
3. Delete a sample-only cluster through `eksctl`. On a shared cluster, remove only sample-owned node resources after confirming no other workloads depend on them.
4. Remove sample-only IAM roles and policies, ECR images or repositories, and Secrets Manager secrets. Use the Secrets Manager recovery window when scheduling deletion.
5. Review remaining storage, log groups, endpoints, and networking for ongoing charges. Remove only resources created exclusively for this sample.

Do not run a blanket `kubectl delete -f deploy/k8s/` on a shared cluster: `05-network.yaml` also targets `kube-system` configuration and cluster-scoped resources. Do not delete a reused VPC, shared subnets, NAT gateways, VPC endpoints, or peering connections. Restore shared configuration from its recorded pre-deployment state where necessary.

## Documentation

This README is in English. Detailed guides, the load-test README, changelog, and many configuration comments are currently in Chinese.

| Document | Contents |
| --- | --- |
| [Configuration](docs/configuration.md) | Fields, defaults, environment expansion, and secret references. |
| [Control plane contract](docs/control-plane.md) | Routes, authorization, usage reporting, and error mappings. |
| [Operations](docs/operations.md) | Adding models and providers, upgrades, metrics, and troubleshooting. |
| [Private networking](docs/private-networking.md) | EKS, IRSA, VPC endpoints, cross-Region, and cross-account setup. |
| [Robustness report](docs/robustness-report.md) | Fault-injection scenarios and test results. |
| [Load testing](loadtest/README.md) | k6 scripts, Kubernetes Jobs, and test-environment measurements. |
| [Changelog](CHANGELOG.md) | Version history and upgrade notes. |

### Repository structure

```text
cmd/gateway/            Gateway entry point and lifecycle
cmd/mock-controlplane/  Test control plane and debug endpoints
internal/config/        Configuration loading and validation
internal/controlplane/  Control plane HTTP client
internal/router/        Route snapshots, polling, and candidate selection
internal/provider/      Provider registry and authentication
internal/secrets/       AWS Secrets Manager resolution
internal/protocol/      Request handling, errors, and usage parsing
internal/proxy/         Inference forwarding, failover, and response relay
internal/metering/      In-memory usage queue and retries
internal/observability/ JSON logging and Prometheus metrics
configs/                Example gateway configurations and routes
deploy/                 Container, EKS, IAM, and Kubernetes examples
docs/                   Detailed guides
scripts/                Live smoke test
loadtest/               k6 scripts, Jobs, and results
```

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for bug reports, feature requests, and pull request guidelines. This project follows the [Amazon Open Source Code of Conduct](CODE_OF_CONDUCT.md).

## Security

If you discover a potential security issue, follow the [security issue notification process](CONTRIBUTING.md#security-issue-notifications). Do not report security vulnerabilities in public GitHub issues.

## License

This sample is licensed under the MIT-0 License. See [LICENSE](LICENSE).

### AWS sample code notice

```text
###################
This sample code is provided to you as AWS Content under the AWS Customer Agreement,
or the relevant written agreement between you and AWS (whichever applies). You should
not use this sample code in your production accounts, or on production, or other
critical data. You are responsible for testing, securing, and optimizing the sample
code as appropriate for production grade use based on your specific quality control
practices and standards.
####################
```
