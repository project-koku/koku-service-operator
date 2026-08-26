package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	kafka "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	scrammech "github.com/segmentio/kafka-go/sasl/scram"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	kafkaReasonBootstrapMissing  = "KafkaBootstrapMissing"
	kafkaReasonUnreachable       = "KafkaUnreachable"
	kafkaReasonSASLSecretInvalid = "KafkaSASLSecretInvalid"
	kafkaReasonTLSCACertInvalid  = "KafkaTLSCACertInvalid"
	kafkaReasonAuthFailed        = "KafkaAuthFailed"
	kafkaReasonTopicMissing      = "KafkaTopicMissing"

	kafkaSASLUsernameKey = "username"
	kafkaSASLPasswordKey = "password"

	kafkaTopicUpload             = "platform.upload.announce"
	kafkaTopicROSEvents          = "hccm.ros.events"
	kafkaTopicROSRecommendations = "rosocp.kruize.recommendations"
	kafkaTopicSourcesEventStream = "platform.sources.event-stream"
)

var kafkaMetadataProbe = defaultKafkaMetadataProbe

type missingKafkaTopicsError struct {
	topics []string
}

func (e *missingKafkaTopicsError) Error() string {
	return fmt.Sprintf("required Kafka topic(s) missing: %s", strings.Join(e.topics, ", "))
}

type kafkaAuthError struct {
	err error
}

func (e *kafkaAuthError) Error() string { return e.err.Error() }
func (e *kafkaAuthError) Unwrap() error { return e.err }

func (r *CostManagementServiceConfigReconciler) validateKafka(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) {
	bootstrapServers := strings.TrimSpace(cfg.Spec.Kafka.BootstrapServers)
	if bootstrapServers == "" {
		r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionFalse,
			kafkaReasonBootstrapMissing, "spec.kafka.bootstrapServers is required")
		return
	}

	dialer, reason, err := r.kafkaDialer(ctx, cfg)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionFalse, reason, err.Error())
		return
	}

	requiredTopics := requiredKafkaTopics(cfg)
	if err := kafkaMetadataProbe(ctx, bootstrapServers, dialer, requiredTopics); err != nil {
		var topicErr *missingKafkaTopicsError
		var authErr *kafkaAuthError
		switch {
		case errors.As(err, &topicErr), errors.Is(err, kafka.UnknownTopicOrPartition):
			r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionFalse,
				kafkaReasonTopicMissing, err.Error())
		case errors.As(err, &authErr):
			r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionFalse,
				kafkaReasonAuthFailed, err.Error())
		default:
			r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionFalse,
				kafkaReasonUnreachable, err.Error())
		}
		return
	}

	r.setCondition(cfg, costv1alpha1.ConditionKafkaReady, metav1.ConditionTrue,
		"KafkaContractValid", strings.Join(requiredTopics, ", "))
}

func requiredKafkaTopics(cfg *costv1alpha1.CostManagementServiceConfig) []string {
	topics := []string{kafkaTopicUpload}
	if costv1alpha1.BoolVal(cfg.Spec.ROS.Enabled, false) {
		topics = append(topics,
			kafkaTopicROSEvents,
			kafkaTopicROSRecommendations,
			kafkaTopicSourcesEventStream,
		)
	}
	return topics
}

func (r *CostManagementServiceConfigReconciler) kafkaDialer(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*kafka.Dialer, string, error) {
	tlsConfig, err := r.kafkaTLSConfig(ctx, cfg)
	if err != nil {
		return nil, kafkaReasonTLSCACertInvalid, err
	}
	saslMechanism, err := r.kafkaSASLMechanism(ctx, cfg)
	if err != nil {
		return nil, kafkaReasonSASLSecretInvalid, err
	}
	return &kafka.Dialer{
		Timeout:       validationTimeout,
		DualStack:     true,
		TLS:           tlsConfig,
		SASLMechanism: saslMechanism,
	}, "", nil
}

func (r *CostManagementServiceConfigReconciler) kafkaTLSConfig(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*tls.Config, error) {
	useTLS := cfg.Spec.Kafka.TLS.Enabled || cfg.Spec.Kafka.SecurityProtocol == "SSL" || cfg.Spec.Kafka.SecurityProtocol == "SASL_SSL"
	if !useTLS {
		return nil, nil
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	caName := strings.TrimSpace(cfg.Spec.Kafka.TLS.CACertSecret)
	if caName == "" {
		return tlsConfig, nil
	}

	caSecret, err := r.getSecret(ctx, cfg.Namespace, caName, []string{caCertKey})
	if err != nil {
		return nil, fmt.Errorf("load Kafka CA Secret: %w", err)
	}
	pool, err := certPoolFromPEM(caSecret.Data[caCertKey])
	if err != nil {
		return nil, fmt.Errorf("secret %q key %q contains no valid PEM certificates", caName, caCertKey)
	}
	tlsConfig.RootCAs = pool
	return tlsConfig, nil
}

func (r *CostManagementServiceConfigReconciler) kafkaSASLMechanism(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (sasl.Mechanism, error) {
	mechanism := strings.TrimSpace(cfg.Spec.Kafka.SASL.Mechanism)
	secretName := strings.TrimSpace(cfg.Spec.Kafka.SASL.ExistingSecret)
	if mechanism == "" && secretName == "" {
		return nil, nil
	}
	if mechanism == "" {
		return nil, fmt.Errorf("spec.kafka.sasl.mechanism is required when spec.kafka.sasl.existingSecret is set")
	}
	if secretName == "" {
		return nil, fmt.Errorf("spec.kafka.sasl.existingSecret is required when spec.kafka.sasl.mechanism is set")
	}

	secret, err := r.getSecret(ctx, cfg.Namespace, secretName, []string{kafkaSASLUsernameKey, kafkaSASLPasswordKey})
	if err != nil {
		return nil, err
	}
	username := string(secret.Data[kafkaSASLUsernameKey])
	password := string(secret.Data[kafkaSASLPasswordKey])

	switch mechanism {
	case "PLAIN":
		return plain.Mechanism{Username: username, Password: password}, nil
	case "SCRAM-SHA-256":
		return scrammech.Mechanism(scrammech.SHA256, username, password)
	case "SCRAM-SHA-512":
		return scrammech.Mechanism(scrammech.SHA512, username, password)
	default:
		return nil, fmt.Errorf("unsupported spec.kafka.sasl.mechanism %q", mechanism)
	}
}

func defaultKafkaMetadataProbe(ctx context.Context, bootstrapServers string, dialer *kafka.Dialer, topics []string) error {
	var last error
	for broker := range strings.SplitSeq(bootstrapServers, ",") {
		broker = strings.TrimSpace(broker)
		if broker == "" {
			continue
		}

		missing := make([]string, 0, len(topics))
		reachable := false
		for _, topic := range topics {
			partitions, err := dialer.LookupPartitions(ctx, "tcp", broker, topic)
			if err != nil {
				if isKafkaAuthError(err) {
					return &kafkaAuthError{err: fmt.Errorf("kafka auth for %q failed: %w", broker, err)}
				}
				if errors.Is(err, kafka.UnknownTopicOrPartition) {
					missing = append(missing, topic)
					reachable = true
					continue
				}
				last = fmt.Errorf("lookup Kafka topic %q via %q: %w", topic, broker, err)
				reachable = false
				break
			}
			reachable = true
			if len(partitions) == 0 || topicMissingFromPartitions(partitions, topic) {
				missing = append(missing, topic)
			}
		}
		if len(missing) > 0 {
			return &missingKafkaTopicsError{topics: missing}
		}
		if reachable {
			return nil
		}
	}
	if last != nil {
		return fmt.Errorf("no usable Kafka broker in %q: %w", bootstrapServers, last)
	}
	return fmt.Errorf("bootstrap-servers %q is empty", bootstrapServers)
}

func topicMissingFromPartitions(partitions []kafka.Partition, topic string) bool {
	for _, partition := range partitions {
		if partition.Topic == topic {
			return false
		}
	}
	return true
}

func isKafkaAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sasl") ||
		strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "not authorized") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "invalid credentials")
}
