package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestToEnrichedEvent(t *testing.T) {
	t.Run("With valid time format", func(t *testing.T) {
		expectedTime, _ := time.Parse(time.RFC3339, "2025-03-03T13:03:29Z")

		properties := map[string]any{
			"value": "12.12",
		}

		event := Event{
			OrganizationID:          "1a901a90-1a90-1a90-1a90-1a901a901a90",
			ExternalSubscriptionID:  "sub_id",
			Code:                    "api_calls",
			Properties:              properties,
			PreciseTotalAmountCents: "100.00",
			Source:                  HTTP_RUBY,
			Timestamp:               1741007009,
		}

		result := event.ToEnrichedEvent()
		assert.True(t, result.Success())

		ere := result.Value()
		assert.Equal(t, event.OrganizationID, ere.OrganizationID)
		assert.Equal(t, event.ExternalSubscriptionID, ere.ExternalSubscriptionID)
		assert.Equal(t, event.Code, ere.Code)
		assert.Equal(t, event.Properties, ere.Properties)
		assert.Equal(t, event.PreciseTotalAmountCents, ere.PreciseTotalAmountCents)
		assert.Equal(t, event.Source, ere.Source)
		assert.Equal(t, 1741007009.0, ere.Timestamp)
		assert.Equal(t, expectedTime, ere.Time)
		assert.Equal(t, map[string]string{}, ere.GroupedBy)
	})

	t.Run("With unsupported time format", func(t *testing.T) {
		event := Event{
			OrganizationID:          "1a901a90-1a90-1a90-1a90-1a901a901a90",
			ExternalSubscriptionID:  "sub_id",
			Code:                    "api_calls",
			PreciseTotalAmountCents: "100.00",
			Source:                  HTTP_RUBY,
			Timestamp:               "2025-03-03 13:03:29",
		}

		result := event.ToEnrichedEvent()
		assert.False(t, result.Success())
		assert.Equal(t, "strconv.ParseFloat: parsing \"2025-03-03 13:03:29\": invalid syntax", result.ErrorMsg())
		assert.False(t, result.Retryable)
	})
}

func TestNotAPIPostProcessed(t *testing.T) {
	t.Run("When event source is not HTTP_RUBY", func(t *testing.T) {
		event := Event{
			Source: "REDPANDA_CONNECT",
		}

		assert.True(t, event.NotAPIPostProcessed())
	})

	t.Run("When event source is HTTP_RUBY without source metadata", func(t *testing.T) {
		event := Event{
			Source: HTTP_RUBY,
		}

		assert.True(t, event.NotAPIPostProcessed())
	})

	t.Run("When event source is HTTP_RUBY with source metadata", func(t *testing.T) {
		event := Event{
			Source: HTTP_RUBY,
			SourceMetadata: &SourceMetadata{
				ApiPostProcess: true,
			},
		}
		assert.False(t, event.NotAPIPostProcessed())

		event.SourceMetadata.ApiPostProcess = false
		assert.True(t, event.NotAPIPostProcessed())
	})
}

func TestActorAttribution(t *testing.T) {
	t.Run("actor from the ingest payload unmarshals onto Event", func(t *testing.T) {
		// This is exactly what the /agent-usage pipeline writes to Kafka:
		// a per-user actor stamped alongside the metered usage.
		raw := `{
			"organization_id": "org-1",
			"external_subscription_id": "sub_id",
			"transaction_id": "tenant-agent-2026081700",
			"code": "agent_tokens",
			"actor": "user:pete@gridironrobotics.com",
			"properties": {"value": "512"},
			"timestamp": 1741007009
		}`

		var event Event
		err := json.Unmarshal([]byte(raw), &event)
		assert.NoError(t, err)
		assert.Equal(t, "user:pete@gridironrobotics.com", event.Actor)
	})

	t.Run("actor is threaded through enrichment", func(t *testing.T) {
		event := Event{
			OrganizationID:         "org-1",
			ExternalSubscriptionID: "sub_id",
			Code:                   "agent_tokens",
			Actor:                  "user:pete@gridironrobotics.com",
			Timestamp:              1741007009,
		}

		result := event.ToEnrichedEvent()
		assert.True(t, result.Success())
		assert.Equal(t, "user:pete@gridironrobotics.com", result.Value().Actor)
	})

	t.Run("actor is additive: legacy events without it stay empty and unaffected", func(t *testing.T) {
		// No "actor" key at all — the pre-existing producer shape.
		raw := `{
			"organization_id": "org-1",
			"external_subscription_id": "sub_id",
			"code": "api_calls",
			"timestamp": 1741007009
		}`

		var event Event
		err := json.Unmarshal([]byte(raw), &event)
		assert.NoError(t, err)
		assert.Equal(t, "", event.Actor)

		result := event.ToEnrichedEvent()
		assert.True(t, result.Success())
		assert.Equal(t, "", result.Value().Actor)

		// omitempty: a legacy event round-trips with no "actor" field, so the
		// change cannot alter an existing payload on the wire.
		out, err := json.Marshal(event)
		assert.NoError(t, err)
		assert.NotContains(t, string(out), "actor")
	})
}
