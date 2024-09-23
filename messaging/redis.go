package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/redis/rueidis"
	"github.com/trinitytechnology/ebrick/config"
	"github.com/trinitytechnology/ebrick/logger"
	"github.com/trinitytechnology/ebrick/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.uber.org/zap"
)

// Initialize logger
func init() {
	log = logger.DefaultLogger
}

// redisStream represents the Redis stream implementation.
type redisStream struct {
	client rueidis.Client
	ctx    context.Context
	subs   []string // Store consumer groups
}

// NewRedisStream creates a new Redis stream instance.
func NewRedisStream(client rueidis.Client) CloudEventStream {
	return &redisStream{
		client: client,
		ctx:    context.Background(),
	}
}

// CreateStream creates a Redis stream if it doesn't already exist.
func (r *redisStream) CreateStream(stream string, subjects []string) error {
	// Attempt to create the stream
	builder := r.client.B().Xadd().Key(stream).Id("*").FieldValue().FieldValue("event", "")
	resp := r.client.Do(r.ctx, builder.Build())

	// Check if the stream already exists
	if resp.Error() != nil && !strings.Contains(resp.Error().Error(), "BUSYKEY") {
		return fmt.Errorf("failed to create stream: %w", resp.Error())
	}

	return nil
}

// CreateConsumerGroup creates a Redis consumer group.
func (r *redisStream) CreateConsumerGroup(stream, group string, config ConsumerConfig) error {
	builder := r.client.B().XgroupCreate().Key(stream).Group(group).Id(config.StartID).Mkstream()
	resp := r.client.Do(r.ctx, builder.Build())
	if resp.Error() != nil && resp.Error().Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("failed to create consumer group: %w", resp.Error())
	}
	return nil
}

// Close closes the Redis connection and unsubscribes from all consumer groups.
func (r *redisStream) Close() error {
	return nil
}

// Publish publishes a CloudEvent to a Redis stream with tracing context.
func (r *redisStream) Publish(ctx context.Context, ev event.Event) error {
	data, err := ev.MarshalJSON()
	if err != nil {
		log.Error("failed to marshal event", zap.Error(err))
		return err
	}

	// Prepare tracing headers
	headers := make(map[string]string)

	// Check if tracing is enabled
	cfg := config.GetConfig().Observability
	if cfg.Tracing.Enable {
		otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(headers))
	}

	// Add event to Redis stream
	builder := r.client.B().Xadd().Key(ev.Type()).Id("*").FieldValue().FieldValue("event", rueidis.BinaryString(data)).FieldValue("trace", headers["trace"])
	resp := r.client.Do(r.ctx, builder.Build())
	if resp.Error() != nil {
		return fmt.Errorf("failed to add message to stream: %w", resp.Error())
	}
	return nil
}

// Subscribe subscribes to a Redis stream and processes incoming CloudEvents.
func (r *redisStream) Subscribe(subject, group string, handler func(ev *event.Event, ctx context.Context) error) error {
	if group == "" {
		return errors.New("group cannot be empty")
	}

	// Start a goroutine to consume messages
	go func() {
		for {
			ev, err := r.ConsumeMessages(group, group, "0", 10, 0, subject)
			if err != nil {
				log.Error("Error consuming messages from stream", zap.Error(err))
				if strings.Contains(err.Error(), "NOGROUP") {
					if err := r.CreateConsumerGroup(subject, group, ConsumerConfig{}); err != nil {
						log.Error("Error creating consumer group:", zap.Error(err))
						return
					}
				}
				continue // Retry on error
			}

			// Process the event with the handler
			if err := handler(&ev, r.ctx); err != nil {
				log.Error("Cannot process event", zap.Any("event", ev), zap.Error(err))
			}
		}
	}()

	log.Info("Successfully subscribed to subject", zap.String("subject", subject), zap.String("group", group))
	return nil
}

// ConsumeMessages consumes messages from the Redis stream.
func (r *redisStream) ConsumeMessages(groupName, consumerName, startID string, count int64, block int64, streams ...string) (event.Event, error) {
	if len(streams) == 0 {
		return event.Event{}, fmt.Errorf("no streams specified")
	}

	streamIDs := make([]string, 0, len(streams))
	for range streams {
		streamIDs = append(streamIDs, startID)
	}

	builder := r.client.B().Xreadgroup().Group(groupName, consumerName).Block(block).Streams().Key(streams...).Id(streamIDs...)
	resp := r.client.Do(r.ctx, builder.Build())
	if resp.Error() != nil {
		return event.Event{}, fmt.Errorf("failed to read messages from stream: %w", resp.Error())
	}

	xEntry, err := resp.AsXRead()
	if err != nil {
		return event.Event{}, fmt.Errorf("failed to read messages from stream: response is not a valid XRead")
	}

	for _, entries := range xEntry {
		for _, fields := range entries {
			if data, ok := fields.FieldValues["event"]; ok {
				// Unmarshal event data
				ev, err := utils.UnmarshalJSON[event.Event](data)
				if err != nil {
					log.Error("failed to unmarshal event", zap.Error(err))
					continue
				}

				// Extract tracing information from fields
				if traceData, ok := fields.FieldValues["trace"]; ok {
					carrier, err := utils.UnmarshalJSON[map[string]string](traceData)
					if err == nil {
						otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(carrier))
					} else {
						log.Error("failed to unmarshal trace data", zap.Error(err))
					}
				}

				return ev, nil
			}
		}
	}

	return event.Event{}, fmt.Errorf("no data found in stream messages")
}

// SubscribeDLQ subscribes to a Dead Letter Queue.
func (r *redisStream) SubscribeDLQ(subject string, handler func(msg any, ctx context.Context) error) error {
	log.Info("Subscribing to Redis DLQ", zap.String("subject", subject))
	return nil
}
