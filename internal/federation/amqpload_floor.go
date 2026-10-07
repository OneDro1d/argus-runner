package federation

// MinExecutorAMQPLoad is the first executor release that carries the `AMQP Load` layer.
// The control plane refuses to queue a set holding such a scenario for an instance that POSITIVELY ranks
// below it (control.amqpLoadEnqueueRefusal) -- friendliness only: an older executor also refuses the
// scenario by itself (unknown layer, and no `status=` bullet). Deliberately LOW: the release PR raises it
// to the tag that actually ships the layer, and a floor set too high would wrongly idle a healthy executor.
const MinExecutorAMQPLoad = "0.3.52"

// MinExecutorHTTPLoad is the first executor release that carries the `HTTP Load` layer, the
// same friendliness floor for the same reason: an older executor refuses the layer by name by itself. 0.3.66 is
// the next release after the 0.3.65 this branch was cut from; the release PR raises it if the layer ships later.
const MinExecutorHTTPLoad = "0.3.66"
