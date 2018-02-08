package kafka

import (
	"context"
	"os"

	"time"

	"sync"

	"fmt"

	"bitbucket.org/ubeedev/kafka-elasticsearch-injector-go/src/models"
	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
)

type kafka struct {
	consumer Consumer
	config   *cluster.Config
	brokers  []string
}

type Consumer struct {
	Topics      []string
	Group       string
	Endpoint    endpoint.Endpoint
	Decoder     DecodeMessageFunc
	Logger      log.Logger
	Concurrency int
	BatchSize   int
}

type topicPartitionOffset struct {
	topic     string
	partition int32
	offset    int64
}

func NewKafka(address string, consumer Consumer) kafka {
	brokers := []string{address}
	config := cluster.NewConfig()
	config.Consumer.Return.Errors = true
	config.Group.Return.Notifications = true

	config.Version = sarama.V0_10_0_0

	return kafka{
		brokers:  brokers,
		config:   config,
		consumer: consumer,
	}
}

func (k *kafka) Start(signals chan os.Signal) {
	topics := k.consumer.Topics
	concurrency := k.consumer.Concurrency
	consumer, err := cluster.NewConsumer(k.brokers, k.consumer.Group, topics, k.config)
	if err != nil {
		panic(err)
	}
	defer consumer.Close()

	// trap SIGINT to trigger a shutdown.

	buffSize := 1
	// Fan-out channel
	consumerCh := make(chan *sarama.ConsumerMessage, buffSize*concurrency*10)
	// Update offset channel
	offsetCh := make(chan *topicPartitionOffset)
	for i := 0; i < concurrency; i++ {
		go func() {
			buf := make([]*sarama.ConsumerMessage, buffSize)
			var decoded []*models.Record
			idx := 0
			for {
				kafkaMsg := <-consumerCh
				level.Info(k.consumer.Logger).Log("bufsize", cap(buf), "buflen", len(buf), "idx", idx)
				buf[idx] = kafkaMsg
				idx++
				for idx == buffSize {
					for _, msg := range buf {
						req, err := k.consumer.Decoder(nil, msg)
						if err != nil {
							level.Error(k.consumer.Logger).Log(
								"message", "Error decoding visit message",
								"err", err.Error(),
							)
							continue
						}
						decoded = append(decoded, req)
					}
					if res, err := k.consumer.Endpoint(context.Background(), decoded); err != nil {
						level.Error(k.consumer.Logger).Log("message", "error on endpoint call", "err", err.Error())
						var _ = res // ignore res (for now)
						continue
					}
					for _, msg := range buf {
						offsetCh <- &topicPartitionOffset{msg.Topic, msg.Partition, msg.Offset}
						consumer.MarkOffset(msg, "") // mark message as processed
					}
					decoded = nil
					idx = 0
				}
			}
		}()
	}
	lock := sync.RWMutex{}
	partitionToOffset := make(map[string]map[int32]int64)
	go func() {
		for {
			offset := <-offsetCh
			lock.Lock()
			currentOffset, exists := partitionToOffset[offset.topic][offset.partition]
			if !exists || offset.offset > currentOffset {
				partitionToOffset[offset.topic][offset.partition] = offset.offset //TODO nil
			}
			lock.Unlock()
		}
	}()

	go func() {
		for range time.Tick(30 * time.Second) { //TODO parameter
			for topic, partitions := range consumer.HighWaterMarks() {
				for partition, maxOffset := range partitions {
					lock.RLock()
					offset, ok := partitionToOffset[topic][partition]
					lock.RUnlock()
					if ok {
						delay := maxOffset - offset
						level.Info(k.consumer.Logger).Log("message", "updating partition offset metric",
							"partition", partition, "maxOffset", maxOffset, "current", offset, "delay", delay)
						updateOffset(topic, partition, delay)
					}
				}
			}
		}
	}()

	// consume messages, watch errors and notifications
	waitTime := 1 * time.Second //TODO parameter
	for {
		if len(consumerCh) > cap(consumerCh) {
			time.Sleep(waitTime) // channel is getting full, wait before pushing more messages
			continue
		}
		select {
		case msg, more := <-consumer.Messages():
			fmt.Println("got message")
			if more {
				consumerCh <- msg
			}
		case err, more := <-consumer.Errors():
			if more {
				level.Error(k.consumer.Logger).Log(
					"message", "Failed to consume message",
					"err", err.Error(),
				)
			}
		case ntf, more := <-consumer.Notifications():
			if more {
				level.Info(k.consumer.Logger).Log(
					"message", "Partitions rebalanced",
					"notification", ntf,
				)
			}
		case <-signals:
			return
		}
	}

}
