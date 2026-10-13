#!/usr/bin/env bash
# The gateway binary in an environment whose STS VPCE has NO private DNS.
# G1: gateway config as shipped (sts_region=Tokyo) -> expected to fail (public STS unreachable).
# G2: same + AWS_ENDPOINT_URL_STS in the pod env -> expected to start and resolve the B identity.
# Private DNS is switched off for the duration and restored at the end. The two running gateway
# replicas keep their cached credentials and are not restarted.
source "$(dirname "$0")/00-env.sh"
: "${A_STS_VPCE_DNS:?run 70-sts-experiments.sh first}"
echo "== disabling private DNS on the Tokyo STS endpoint"
awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --no-private-dns-enabled >/dev/null
until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].State' --output text)" = available ]; do sleep 5; done
sleep 75
echo; echo "################ G1 gateway as shipped, STS VPCE without private DNS"
KEEP_STS_REGION=yes WAIT=60 bash "$HERE/75-gateway-no-sts-region.sh" 2>&1 | tee "$OUT/gateway-no-private-dns-G1.log"
echo; echo "################ G2 same + AWS_ENDPOINT_URL_STS=https://$A_STS_VPCE_DNS"
KEEP_STS_REGION=yes WAIT=45 STS_ENDPOINT_URL="https://$A_STS_VPCE_DNS" bash "$HERE/75-gateway-no-sts-region.sh" 2>&1 | tee "$OUT/gateway-no-private-dns-G2.log"
echo; echo "== re-enabling private DNS on the Tokyo STS endpoint"
awsA ec2 modify-vpc-endpoint --vpc-endpoint-id "$A_VPCE_STS" --private-dns-enabled >/dev/null
until [ "$(awsA ec2 describe-vpc-endpoints --vpc-endpoint-ids "$A_VPCE_STS" --query 'VpcEndpoints[0].PrivateDnsEnabled' --output text)" = True ]; do sleep 5; done
echo "private DNS restored"
