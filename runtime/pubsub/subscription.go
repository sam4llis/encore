package pubsub

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"encore.dev/appruntime/api"
	"encore.dev/appruntime/config"
	"encore.dev/appruntime/model"
	"encore.dev/appruntime/reqtrack"
	"encore.dev/appruntime/testsupport"
	"encore.dev/appruntime/trace"
	"encore.dev/beta/errs"
	"encore.dev/pubsub/internal/gcp"
	"encore.dev/pubsub/internal/nsq"
	"encore.dev/pubsub/internal/utils"
)

type Manager struct {
	ctx        context.Context
	cancelCtx  func()
	cfg        *config.Config
	rt         *reqtrack.RequestTracker
	ts         *testsupport.Manager
	rootLogger zerolog.Logger
	gcp        *gcp.Manager
	nsq        *nsq.Manager

	publishCounter uint64

	outstanding *outstandingMessageTracker
}

func NewManager(cfg *config.Config, rt *reqtrack.RequestTracker, ts *testsupport.Manager, server *api.Server, rootLogger zerolog.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	gcpMgr := gcp.NewManager(ctx, cfg, server)
	nsqMgr := nsq.NewManager(ctx, cfg, rt)
	return &Manager{
		ctx:        ctx,
		cancelCtx:  cancel,
		cfg:        cfg,
		rt:         rt,
		ts:         ts,
		rootLogger: rootLogger,
		gcp:        gcpMgr,
		nsq:        nsqMgr,
	}
}

func (mgr *Manager) Shutdown(force context.Context) {
	mgr.cancelCtx()
	mgr.outstanding.ArmForShutdown()

	select {
	case <-mgr.outstanding.Done():
	case <-force.Done():
	}
}

// Subscription represents a subscription to a Topic.
type Subscription[T any] struct {
	mgr *Manager
}

// NewSubscription is used to declare a Subscription to a topic. The passed in handler will be called
// for each message published to the topic.
//
// A call to NewSubscription can only be made when declaring a package level variable. Any
// calls to this function made outside a package level variable declaration will result
// in a compiler error.
//
// The subscription name must be unique for that topic. Subscription names must be defined
// in kebab-case (lowercase alphanumerics and hyphen seperated). The subscription name must start with a letter
// and end with either a letter or number. It cannot be longer than 63 characters.
//
// Once created and deployed never change the subscription name, or the topic name otherwise messages will be lost which
// could be in flight.
//
// Example:
//
//     import "encore.dev/pubsub"
//
//     type MyEvent struct {
//       Foo string
//     }
//
//     var MyTopic = pubsub.NewTopic[*MyEvent]("my-topic", pubsub.TopicConfig{
//       DeliveryGuarantee: pubsub.AtLeastOnce,
//     })
//
//     var Subscription = pubsub.NewSubscription(MyTopic, "my-subscription", pubsub.SubscriptionConfig[*MyEvent]{
//       Handler:     HandleEvent,
//       RetryPolicy: &pubsub.RetryPolicy { MaxRetries: 10 },
//     })
//
//     func HandleEvent(ctx context.Context, event *MyEvent) error {
//       rlog.Info("received foo")
//       return nil
//     }
func NewSubscription[T any](topic *Topic[T], name string, subscriptionCfg SubscriptionConfig[T]) *Subscription[T] {
	if topic.topicCfg == nil || topic.topic == nil || topic.mgr == nil {
		panic("pubsub topic was not created using pubsub.NewTopic")
	}
	mgr := topic.mgr

	// Set default config values for missing values
	if subscriptionCfg.RetryPolicy == nil {
		subscriptionCfg.RetryPolicy = &RetryPolicy{
			MaxRetries: 100,
		}
	}
	if subscriptionCfg.RetryPolicy.MinBackoff < 0 {
		panic("MinRetryDelay cannot be negative")
	}
	if subscriptionCfg.RetryPolicy.MaxBackoff < 0 {
		panic("MaxRetryDelay cannot be negative")
	}
	subscriptionCfg.RetryPolicy.MinBackoff = utils.WithDefaultValue(subscriptionCfg.RetryPolicy.MinBackoff, 10*time.Second)
	subscriptionCfg.RetryPolicy.MaxBackoff = utils.WithDefaultValue(subscriptionCfg.RetryPolicy.MaxBackoff, 10*time.Minute)

	subscription, staticCfg := topic.getSubscriptionConfig(name)
	panicCatchWrapper := func(ctx context.Context, msg T) (err error) {
		defer func() {
			if err2 := recover(); err2 != nil {
				err = errs.B().Code(errs.Internal).Msgf("subscriber panicked: %s", err2).Err()
			}
		}()

		return subscriptionCfg.Handler(ctx, msg)
	}

	log := mgr.rootLogger.With().
		Str("service", staticCfg.Service).
		Str("topic", topic.topicCfg.EncoreName).
		Str("subscription", name).
		Logger()

	tracingEnabled := trace.Enabled(mgr.cfg)

	// Subscribe to the topic
	topic.topic.Subscribe(&log, subscriptionCfg.RetryPolicy, subscription, func(ctx context.Context, msgID string, publishTime time.Time, deliveryAttempt int, attrs map[string]string, data []byte) (err error) {
		mgr.outstanding.Inc()
		defer mgr.outstanding.Dec()

		if !mgr.cfg.Static.Testing {
			// Under test we're already inside an operation
			mgr.rt.BeginOperation()
			defer mgr.rt.FinishOperation()
		}

		msg, err := utils.UnmarshalMessage[T](attrs, data)
		if err != nil {
			log.Err(err).Str("msg_id", msgID).Int("delivery_attempt", deliveryAttempt).Msg("failed to unmarshal message")
			return errs.B().Code(errs.Internal).Cause(err).Msg("failed to unmarshal message").Err()
		}

		// Start the request tracing span
		req := &model.Request{
			Type:    model.PubSubMessage,
			Service: staticCfg.Service,
			MsgData: &model.PubSubMsgData{
				Topic:        topic.topicCfg.EncoreName,
				Subscription: subscription.EncoreName,
				MessageID:    msgID,
				Attempt:      deliveryAttempt,
				Published:    publishTime,
			},
			Inputs: [][]byte{data},
			DefLoc: staticCfg.TraceIdx,
			Traced: tracingEnabled,

			// Unset for subscriptions
			UID:      "",
			AuthData: nil,
			ParentID: model.SpanID{},
		}
		req.Logger = &log

		mgr.rt.BeginRequest(req)
		curr := mgr.rt.Current()
		if curr.Trace != nil {
			curr.Trace.BeginRequest(req, curr.Goctr)
		}

		err = panicCatchWrapper(ctx, msg)

		if curr.Trace != nil {
			curr.Trace.FinishRequest(req, nil, err)
		}
		mgr.rt.FinishRequest()

		return err
	})

	if !mgr.cfg.Static.Testing {
		// Log the subscription registration - unless we're in unit tests
		log.Info().Msg("registered subscription")
	}

	return &Subscription[T]{mgr: mgr}
}

func (t *Topic[T]) getSubscriptionConfig(name string) (*config.PubsubSubscription, *config.StaticPubsubSubscription) {
	if t.mgr.cfg.Static.Testing {
		// No subscriptions occur in testing
		return &config.PubsubSubscription{EncoreName: name}, &config.StaticPubsubSubscription{
			Service: t.mgr.cfg.Static.TestService,
		}
	}

	// Fetch the subscription configuration
	subscription, ok := t.topicCfg.Subscriptions[name]
	if !ok {
		t.mgr.rootLogger.Fatal().Msgf("unregistered/unknown subscription on topic %s: %s", t.topicCfg.EncoreName, name)
	}

	staticCfg, ok := t.mgr.cfg.Static.PubsubTopics[t.topicCfg.EncoreName].Subscriptions[name]
	if !ok {
		t.mgr.rootLogger.Fatal().Msgf("unregistered/unknown subscription on topic %s: %s", t.topicCfg.EncoreName, name)
	}

	return subscription, staticCfg
}

// outstandingMessageTracker tracks the number of outstanding messages.
// Once Shutdown() has been called, the next time the number of outstanding
// messages reaches zero (or if it's already zero), the Done() channel is closed.
type outstandingMessageTracker struct {
	active int64

	closeOnce sync.Once
	done      chan struct{}

	mu    sync.Mutex
	armed bool
}

func newOutstandingMessageTracker() *outstandingMessageTracker {
	return &outstandingMessageTracker{
		done: make(chan struct{}),
	}
}

func (t *outstandingMessageTracker) Inc() {
	atomic.AddInt64(&t.active, 1)
}

func (t *outstandingMessageTracker) Dec() {
	val := atomic.AddInt64(&t.active, -1)
	if val < 0 {
		panic("outstandingMessageTracker: active < 0")
	}
	if val == 0 {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.armed {
			t.markDone()
		}
	}
}

func (t *outstandingMessageTracker) ArmForShutdown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = true

	// If we're already at zero, mark it as done right away.
	if atomic.LoadInt64(&t.active) == 0 {
		t.markDone()
	}
}

// markDone marks the tracker as being done.
func (t *outstandingMessageTracker) markDone() {
	t.closeOnce.Do(func() { close(t.done) })
}

func (t *outstandingMessageTracker) Done() <-chan struct{} {
	return t.done
}