// Copyright (c) 2023 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/client"
	ceprotocol "github.com/cloudevents/sdk-go/v2/protocol"
	"github.com/cloudevents/sdk-go/v2/protocol/gochan"
	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stolostron/multicluster-global-hub/pkg/database"
	"github.com/stolostron/multicluster-global-hub/pkg/database/models"
	"github.com/stolostron/multicluster-global-hub/pkg/transport"
	"github.com/stolostron/multicluster-global-hub/pkg/transport/config"
	"github.com/stolostron/multicluster-global-hub/pkg/transport/kafka_confluent"
)

var transportID string

type GenericConsumer struct {
	log                  logr.Logger
	client               cloudevents.Client
	assembler            *messageAssembler
	eventChan            chan *cloudevents.Event
	consumeTopics        []string
	clusterIdentity      string
	enableDatabaseOffset bool
}

type GenericConsumeOption func(*GenericConsumer) error

func EnableDatabaseOffset(enableOffset bool) GenericConsumeOption {
	return func(c *GenericConsumer) error {
		c.enableDatabaseOffset = enableOffset
		return nil
	}
}

func NewGenericConsumer(tranConfig *transport.TransportConfig, topics []string,
	opts ...GenericConsumeOption,
) (*GenericConsumer, error) {
	log := ctrl.Log.WithName(fmt.Sprintf("%s-consumer", tranConfig.TransportType))
	var receiver interface{}
	var err error
	var clusterIdentity string
	switch tranConfig.TransportType {
	case string(transport.Kafka):
		log.Info("transport consumer with cloudevents-kafka receiver")
		receiver, err = getConfluentReceiverProtocol(tranConfig, topics)
		if err != nil {
			return nil, err
		}
		clusterIdentity = tranConfig.KafkaConfig.ClusterIdentity
	case string(transport.Chan):
		log.Info("transport consumer with go chan receiver")
		if tranConfig.Extends == nil {
			tranConfig.Extends = make(map[string]interface{})
		}
		topic := "event"
		if topics != nil && len(topics) > 0 {
			topic = topics[0]
		}
		if _, found := tranConfig.Extends[topic]; !found {
			tranConfig.Extends[topic] = gochan.New()
		}
		receiver = tranConfig.Extends[topic]
		clusterIdentity = "kafka-cluster-chan"
	default:
		return nil, fmt.Errorf("transport-type - %s is not a valid option", tranConfig.TransportType)
	}

	client, err := cloudevents.NewClient(receiver, client.WithPollGoroutines(1))
	if err != nil {
		return nil, err
	}

	c := &GenericConsumer{
		log:                  log,
		client:               client,
		clusterIdentity:      clusterIdentity,
		eventChan:            make(chan *cloudevents.Event),
		assembler:            newMessageAssembler(),
		enableDatabaseOffset: false,
		consumeTopics:        topics,
	}
	if err := c.applyOptions(opts...); err != nil {
		return nil, err
	}
	transportID = clusterIdentity
	return c, nil
}

func (c *GenericConsumer) applyOptions(opts ...GenericConsumeOption) error {
	for _, fn := range opts {
		if err := fn(c); err != nil {
			return err
		}
	}
	return nil
}

func (c *GenericConsumer) Start(ctx context.Context) error {
	receiveContext := ctx
	if c.enableDatabaseOffset {
		offsets, err := getInitOffset(c.clusterIdentity)
		if err != nil {
			return err
		}
		c.log.Info("init consumer", "offsets", offsets)
		if len(offsets) > 0 {
			receiveContext = kafka_confluent.WithTopicPartitionOffsets(ctx, offsets)
		}
	}

	err := c.client.StartReceiver(receiveContext, func(ctx context.Context, event cloudevents.Event) ceprotocol.Result {
		c.log.V(2).Info("received message", "event.Source", event.Source(), "event.Type", event.Type())

		chunk, isChunk := c.assembler.messageChunk(event)
		if !isChunk {
			c.eventChan <- &event
			return ceprotocol.ResultACK
		}
		if payload := c.assembler.assemble(chunk); payload != nil {
			if err := event.SetData(cloudevents.ApplicationJSON, payload); err != nil {
				c.log.Error(err, "failed the set the assembled data to event")
			} else {
				c.eventChan <- &event
			}
		}
		return ceprotocol.ResultACK
	})
	if err != nil {
		return fmt.Errorf("failed to start Receiver: %w", err)
	}
	c.log.Info("receiver stopped\n")
	return nil
}

func (c *GenericConsumer) EventChan() chan *cloudevents.Event {
	return c.eventChan
}

func getInitOffset(kafkaClusterIdentity string) ([]kafka.TopicPartition, error) {
	db := database.GetGorm()
	var positions []models.Transport
	err := db.Where("name ~ ?", "^status*").
		Where("payload->>'ownerIdentity' <> ? AND payload->>'ownerIdentity' = ?", "", kafkaClusterIdentity).
		Find(&positions).Error
	if err != nil {
		return nil, err
	}
	offsetToStart := []kafka.TopicPartition{}
	for i, pos := range positions {
		var kafkaPosition transport.EventPosition
		err := json.Unmarshal(pos.Payload, &kafkaPosition)
		if err != nil {
			return nil, err
		}
		offsetToStart = append(offsetToStart, kafka.TopicPartition{
			Topic:     &positions[i].Name,
			Partition: kafkaPosition.Partition,
			Offset:    kafka.Offset(kafkaPosition.Offset),
		})
	}
	return offsetToStart, nil
}

// func getSaramaReceiverProtocol(transportConfig *transport.TransportConfig) (interface{}, error) {
// 	saramaConfig, err := config.GetSaramaConfig(transportConfig.KafkaConfig)
// 	if err != nil {
// 		return nil, err
// 	}
// 	// if set this to false, it will consume message from beginning when restart the client
// 	saramaConfig.Consumer.Offsets.AutoCommit.Enable = true
// 	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest
// 	// set the consumer groupId = clientId
// 	return kafka_sarama.NewConsumer([]string{transportConfig.KafkaConfig.BootstrapServer}, saramaConfig,
// 		transportConfig.KafkaConfig.ConsumerConfig.ConsumerID,
// 		transportConfig.KafkaConfig.ConsumerConfig.ConsumerTopic)
// }

func getConfluentReceiverProtocol(transportConfig *transport.TransportConfig, topics []string) (interface{}, error) {
	configMap, err := config.GetConfluentConfigMap(transportConfig.KafkaConfig, false)
	if err != nil {
		return nil, err
	}

	return kafka_confluent.New(kafka_confluent.WithConfigMap(configMap),
		kafka_confluent.WithReceiverTopics(topics))
}

func TransportID() string {
	return transportID
}
