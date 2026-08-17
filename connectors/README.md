# Lago Ingest Connectors

## Events format

- All events should respect the same format as [Lago API](https://doc.getlago.com/api-reference/events/usage)

```json
{
  "event": {
    "external_subscription_id": "string",
    "transaction_id": "unique_transaction_identifier",
    "code": "billable_metric_code",
    // Event Unix Timestamp
    "timestamp": 1620000000,
    "properties": {
      // Should respect the format of your billable metric
      "my_property": "my_value"
    },
    // Optional, used for the dynamic pricing feature, defaulted to 0
    "precise_total_amount_cents": 1000
  }
}
```

## Configuration

### Environment Variables

|Environment Variable|Description|Required|
|---|---|---|
|LOG_LEVEL|Log level for your connector, default: info|No|

## SQS Connector

### Environment Variables

|Environment Variable|Description|Required|
|---|---|---|
|SQS_ENDPOINT|The endpoint of the SQS service|Yes|
|SQS_REGION|The region of the SQS service|Yes|
|SQS_KEY_ID|The AWS access key id|Yes|
|SQS_KEY_SECRET|The AWS secret key|Yes|
|SQS_DLQ_ENDPOINT|The endpoint of the SQS service for the dead letter queue|No|
|ORGANIZATION_ID|Lago organization ID|Yes|
|KAFKA_BROKERS|Redpanda Broker|Yes|
|KAFKA_USER|Redpanda User|Yes|
|KAFKA_PASSWORD|Redpanda Password|Yes|
|KAFKA_TOPIC|Redpanda Topic to send events|Yes|

## Kinesis Connector

### Environment Variables

|Environment Variable|Description|Required|
|---|---|---|
|KINESIS_STREAM|The name of the Kinesis stream to consume|Yes|
|AWS_REGION|The AWS region for Kinesis and DynamoDB|Yes|
|AWS_ROLE|The IAM role ARN to assume for accessing Kinesis|Yes|
|AWS_ROLE_EXTERNAL_ID|The external ID for the assumed role|Yes|
|DYNAMODB_TABLE|The DynamoDB table name used for checkpointing|Yes|
|KAFKA_BROKERS|Redpanda Broker|Yes|
|KAFKA_USER|Redpanda User|Yes|
|KAFKA_PASSWORD|Redpanda Password|Yes|
|KAFKA_TOPIC|Redpanda Topic to send events|Yes|
|KAFKA_TLS|Enable TLS for Kafka connection, default: false|No|
|KAFKA_BATCH_COUNT|Number of messages to batch before sending, default: 100|No|
|KAFKA_BATCH_BYTE_SIZE|Maximum size in bytes for batching, default: 1000000 (1MB)|No|
|KAFKA_BATCH_PERIOD|Time period for batching, default: 1s|No|

## HTTP Server

Like the SQS and Kinesis connectors, the HTTP ingestor is **per-tenant**: the
organization is set server-side from `ORGANIZATION_ID`, never taken from the
request body — so a caller cannot inject events for another organization. The
`/events` endpoint is **authenticated** and fail-closed: the caller must present
`Authorization: Bearer $INGEST_TOKEN`; a request with a missing or wrong token is
answered `401` and its event is **dropped before it can reach Kafka**. With
`INGEST_TOKEN` unset the pipeline rejects everything (fail-closed by default).

> The auth check runs **in-pipeline** (a Bloblang processor comparing the bearer),
> not via a `basic_auth:` block. The redpanda-connect `http_server` input has no
> native auth field — an earlier `basic_auth:` block was silently invalid and made
> the pipeline refuse to boot under the pinned connect image. Bearer-in-pipeline is
> the mechanism that actually authenticates this input.
>
> `INGEST_TOKEN` is interpolated into the comparison the same way `KAFKA_PASSWORD`
> is (a `${…}` reference), so use an **opaque, URL-safe token** — no quotes, spaces,
> or backslashes. A pathological value fails loudly at boot (parse error), never
> silently; and because it is an operator secret, not caller input, it is not an
> injection vector.

### Environment Variables

|Environment Variable|Description|Required|
|---|---|---|
|ORGANIZATION_ID|Lago organization ID this ingestor writes events for|Yes|
|INGEST_TOKEN|Shared bearer the caller presents as `Authorization: Bearer …`. Unset ⇒ all requests rejected (fail-closed)|Yes|
|INGEST_AUTH_ENABLED|`false` disables auth for trusted-network dev **only**; anything else (default) enforces it|No|
|KAFKA_BROKERS|Redpanda Broker|Yes|
|KAFKA_USER|Redpanda User|Yes|
|KAFKA_PASSWORD|Redpanda Password|Yes|
|KAFKA_TOPIC|Redpanda Topic to send events|Yes|
|KAFKA_TLS|Enable TLS for Kafka connection, default: false|No|
|KAFKA_BATCH_COUNT|Number of messages to batch before sending, default: 100|No|
|KAFKA_BATCH_BYTE_SIZE|Maximum size in bytes for batching, default: 1000000 (1MB)|No|
|KAFKA_BATCH_PERIOD|Time period for batching, default: 1s|No|

## Agent-usage ingest (`/agent-usage`)

Workstream C: the agent plane's metering feed. `agent_usage.yml` is a second HTTP
ingestor, identical in shape to the `/events` one above but on the `/agent-usage`
path, dedicated to billing the AI-agent estate. The langgraph cost governor
(`langgraph-agents`) emits per-tenant token and tool-call counts and this pipeline
turns them into Lago usage events. It shares the `INGEST_TOKEN` /
`INGEST_AUTH_ENABLED` credential and the `ORGANIZATION_ID` / `KAFKA_*` env with
`http.yml` (same trusted ingest boundary, same fail-closed bearer auth, same
server-side org stamp), so no new environment variables are introduced.

**Per-actor attribution (§4 per-user metering).** The pipeline passes an optional
`actor` field through verbatim from the producer's audit record (the `actor`
stamped on every `routing_decision` + `tool_invoke`), landing it on the Kafka
payload alongside `organization_id`. The events-processor `Event`/`EnrichedEvent`
models parse it (`actor`, `omitempty`) so per-user usage survives to the billing
sink. It is additive: a producer that sends no `actor` is unchanged, and the
tenant boundary stays server-side (`organization_id`) — `actor` is a sub-dimension
*within* the tenant, never a way to select another tenant.

**Lago-side setup (do this first).** Nothing meters until the billable metrics the
producer names exist on the organisation. Create at least:

|Billable metric `code`|Aggregation|`properties` field|
|---|---|---|
|`agent_tokens`|`sum_agg`|`value` (tokens for the turn/bucket)|
|`agent_tool_calls`|`count_agg`|— (one event per tool call, or `value` per bucket)|

**Idempotency (the rule this feed lives or dies by).** `transaction_id` is passed
through verbatim and must be **deterministic on the producer side** — derive it
from stable inputs (e.g. `tenant + subsystem + hourly bucket`), never from a fresh
UUID per delivery. Lago deduplicates on it, so replaying the same id is safe; a
retry that mints a *new* id silently double-bills the customer. Recommended v1
grain is the hourly rollup, which matches the governor's existing hourly bucketing
and yields the deterministic id for free.

The producer side (the cost-governor sink that POSTs here) lives in the
`langgraph-agents` repo and ships in that repo's PR — see its `cost_governor.py`
`governed_record()` fan-out. This repo owns only the ingest pipeline.

