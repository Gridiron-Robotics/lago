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
`/events` endpoint is protected by **HTTP Basic auth** (fail-closed: enabled by
default; set the credentials before exposing it). Provide a password *hash*, not
the plaintext — e.g. `echo -n "$PASS$SALT" | sha256sum` for `sha256`, or a
`bcrypt` hash (with `INGEST_PASSWORD_ALGORITHM=bcrypt`, no salt).

### Environment Variables

|Environment Variable|Description|Required|
|---|---|---|
|ORGANIZATION_ID|Lago organization ID this ingestor writes events for|Yes|
|INGEST_USERNAME|Basic-auth username for `POST /events`|Yes|
|INGEST_PASSWORD_HASH|Basic-auth password **hash** (see `INGEST_PASSWORD_ALGORITHM`)|Yes|
|INGEST_PASSWORD_ALGORITHM|Hash algorithm: `sha256` (default), `md5`, `bcrypt`, `scrypt`|No|
|INGEST_PASSWORD_SALT|Salt for `sha256`/`scrypt` (not used for `bcrypt`)|If sha256/scrypt|
|INGEST_BASIC_AUTH_ENABLED|Disable auth only for trusted-network dev, default: true|No|
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
turns them into Lago usage events. It shares the `INGEST_*` credentials and the
`ORGANIZATION_ID` / `KAFKA_*` env with `http.yml` (same trusted ingest boundary,
same fail-closed Basic auth, same server-side org stamp), so no new environment
variables are introduced.

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

