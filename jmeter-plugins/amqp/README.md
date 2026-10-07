# JMeter AMQP Sampler

A JMeter Java Request sampler plugin for AMQP 0-9-1 brokers (RabbitMQ). The OneDroid Argus runner drives it
through `templates/message-flow.jmx` and `templates/amqp-load.jmx`. The shaded jar goes into `/opt/jmeter/lib/ext`
in the runner image (see the repository `Dockerfile`).

Build and unit-test it with Maven (no broker needed):

```sh
cd jmeter-plugins/amqp
mvn -B package
```

Tests tagged `live` need a broker and are excluded by default; `mvn -Plive test` runs only those, against the broker
named by the `ARGUS_TEST_AMQP_*` variables.

## Samplers

- **AmqpPublishSampler** — publish messages to any exchange, measure publish throughput
- **AmqpConsumeSampler** — consume messages with optional correlationId / routing-key filtering
- **AmqpRpcSampler** — request-reply with an exclusive reply queue, measures round-trip latency
- **AmqpQueueDepthSampler** — non-destructive queue depth check via passive declare, for post-publish assertions
- **AmqpFanoutSampler** — publish once to a fanout exchange and verify delivery to every bound queue
- **AmqpDirectReplySampler** — RabbitMQ request-reply via the `amq.rabbitmq.reply-to` pseudo-queue
- **AmqpPubSubSampler** — publish to a topic exchange and verify positive and negative routing assertions

The Java package is `com.onedroid.jmeter.amqp`. See `HISTORY.md` for the licence note.
