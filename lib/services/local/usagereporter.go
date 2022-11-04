/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package local

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"

	prehogapi "github.com/gravitational/prehog/gen/proto/prehog/v1alpha"
	prehogclient "github.com/gravitational/prehog/gen/proto/prehog/v1alpha/prehogv1alphaconnect"
)

const (
	// usageReporterMinBatchSize determines the size at which a batch is sent
	// regardless of elapsed time
	//usageReporterMinBatchSize = 100
	usageReporterMinBatchSize = 5

	// usageReporterMaxBatchSize is the largest batch size that will be sent to
	// the server; batches larger than this will be split into multiple
	// requests.
	//usageReporterMaxBatchSize = 500
	usageReporterMaxBatchSize = 10

	// usageReporterMaxBatchAge is the maximum age a batch may reach before
	// being flushed, regardless of the batch size
	//usageReporterMaxBatchAge = time.Minute * 5
	usageReporterMaxBatchAge = time.Second * 30

	// usageReporterMaxBufferSize is the maximum size to which the event buffer
	// may grow. Events submitted once this limit is reached will be discarded.
	// Events that were in the submission queue that fail to submit may also be
	// discarded when requeued.
	//usageReporterMaxBufferSize = 1000
	usageReporterMaxBufferSize = 20
)

// DiscardUsageReporter is a dummy usage reporter that drops all events.
type DiscardUsageReporter struct{}

func (d *DiscardUsageReporter) SubmitAnonymizedUsageEvents(event ...services.UsageAnonymizable) error {
	// do nothing
	return nil
}

// NewDiscardUsageReporter creates a new usage reporter that drops all events.
func NewDiscardUsageReporter() *DiscardUsageReporter {
	return &DiscardUsageReporter{}
}

// submitFunc is a func that submits a batch of usage events.
type usageSubmitFunc func(reporter *UsageReporter, batch []*prehogapi.SubmitEventRequest) error

type UsageReporter struct {
	// Entry is a log entry
	*log.Entry

	clock clockwork.Clock

	ctx context.Context

	cancel context.CancelFunc

	// anonymizer is the anonymizer used for filtered audit events.
	anonymizer utils.Anonymizer

	// events receives batches incoming events from various Teleport components
	events chan []*prehogapi.SubmitEventRequest

	// buf stores events for batching
	buf []*prehogapi.SubmitEventRequest

	// submissionQueue queues events for submission
	submissionQueue chan []*prehogapi.SubmitEventRequest

	// submit is the func that submits batches of events to a backend
	submit usageSubmitFunc

	// clusterName is the cluster's name, used for anonymization and as an event
	// field.
	clusterName types.ClusterName

	// minBatchSize is the minimum batch size
	minBatchSize int
	maxBatchSize int
	maxBatchAge  time.Duration

	// maxBufferSize is the maximum number of events that can be queued in the
	// buffer.
	maxBufferSize int
}

// runSubmit starts the submission thread. It should be run as a background
// goroutine to ensure SubmitAnonymizedUsageEvents() never blocks.
func (r *UsageReporter) runSubmit() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case batch := <-r.submissionQueue:
			if err := r.submit(r, batch); err != nil {
				r.Warnf("Failed to submit batch of %d usage events: %v", len(batch), err)

				// Put the failed events back on the queue.
				r.resubmitEvents(batch)
			}
		}
	}
}

// enqueueBatch prepares a batch for submission, removing it from the buffer and
// adding it to the submission queue.
func (r *UsageReporter) enqueueBatch() {
	if len(r.buf) == 0 {
		// Nothing to do.
		return
	}

	var events []*prehogapi.SubmitEventRequest
	var remaining []*prehogapi.SubmitEventRequest
	if len(r.buf) > r.maxBatchSize {
		// Split the request and send the first batch. Any remaining events will
		// sit in the buffer to send with the next batch.
		events = r.buf[:r.maxBatchSize]
		remaining = r.buf[r.maxBatchSize:]
	} else {
		// The event buf is small enough to send in one request. We'll replace
		// the buf to allow any excess memory from the last buf to be GC'd.
		events = r.buf
		remaining = make([]*prehogapi.SubmitEventRequest, 0, r.minBatchSize)
	}

	select {
	case r.submissionQueue <- events:
		// Wrote to the queue successfully, so swap buf with the shortened one.
		r.buf = remaining
	default:
		// The queue is full, we'll try again later. Leave the existing buf in
		// place.
	}
}

func (r *UsageReporter) Run() {
	r.Warnf("Started usage reporter")

	// Also start the submission goroutine.
	go r.runSubmit()

	timer := r.clock.NewTimer(r.maxBatchAge)

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.Chan():
			// Once the timer triggers, send any non-empty batch.
			timer.Reset(r.maxBatchAge)
			r.enqueueBatch()
		case events := <-r.events:
			// If the buffer's already full, just warn and discard.
			if len(r.buf) >= r.maxBufferSize {
				// TODO: What level should we log usage submission errors at?
				r.Warnf("Usage event buffer is full, %d events will be discarded", len(events))
				break
			}

			if len(r.buf)+len(events) > r.maxBufferSize {
				keep := r.maxBufferSize - len(r.buf)
				r.Warnf("Usage event buffer is full, %d events will be discarded", len(events)-keep)
				events = events[:keep]
			}

			r.buf = append(r.buf, events...)

			// If we've accumulated enough events to trigger an early send, do
			// so and reset the timer.
			if len(r.buf) >= r.minBatchSize {
				timer.Reset(r.maxBatchAge)
				r.enqueueBatch()
			}
		}
	}
}

// convertEvent anonymizes an event and converts a UsageAnonymizable into a
// SubmitEventRequest.
func (r *UsageReporter) convertEvent(event services.UsageAnonymizable) (*prehogapi.SubmitEventRequest, error) {
	event.Anonymize(r.anonymizer)

	time := timestamppb.New(r.clock.Now())
	clusterName := r.anonymizer.Anonymize([]byte(r.clusterName.GetClusterName())) // TODO: Verify this.

	// "Event" can't be named because protoc doesn't export the interface, so
	// instead we have a giant, fallible switch statement for something the
	// compiler could just as well check for us >:(
	switch e := event.(type) {
	case *services.UsageUserLogin:
		return &prehogapi.SubmitEventRequest{
			ClusterName: clusterName,
			Timestamp:   time,
			Event: &prehogapi.SubmitEventRequest_UserLogin{
				UserLogin: (*prehogapi.UserLoginEvent)(e),
			},
		}, nil
	case *services.UsageSSOCreate:
		return &prehogapi.SubmitEventRequest{
			ClusterName: clusterName,
			Timestamp:   time,
			Event: &prehogapi.SubmitEventRequest_SsoCreate{
				SsoCreate: (*prehogapi.SSOCreateEvent)(e),
			},
		}, nil
	case *services.UsageSessionStart:
		return &prehogapi.SubmitEventRequest{
			ClusterName: clusterName,
			Timestamp:   time,
			Event: &prehogapi.SubmitEventRequest_SessionStart{
				SessionStart: (*prehogapi.SessionStartEvent)(e),
			},
		}, nil
	default:
		return nil, trace.BadParameter("unexpected event usage type %T", event)
	}
}

func (r *UsageReporter) SubmitAnonymizedUsageEvents(events ...services.UsageAnonymizable) error {
	var anonymized []*prehogapi.SubmitEventRequest

	for _, e := range events {
		e.Anonymize(r.anonymizer)

		converted, err := r.convertEvent(e)
		if err != nil {
			return trace.Wrap(err)
		}

		anonymized = append(anonymized, converted)
	}

	r.events <- anonymized

	return nil
}

// resubmitEvents resubmits events that have already been processed (in case of
// some error during submission).
func (r *UsageReporter) resubmitEvents(events []*prehogapi.SubmitEventRequest) {
	r.events <- events
}

// dummySubmit is a submit impl that only logs events.
func dummySubmit(reporter *UsageReporter, events []*prehogapi.SubmitEventRequest) error {
	l := log.WithFields(log.Fields{
		trace.Component: teleport.Component(teleport.ComponentUsageReporting),
	})

	stringified := ""
	for _, ev := range events {
		stringified += fmt.Sprintf("%+v ", ev)
	}

	l.Warnf("dummy submit %d: %+v", len(events), stringified)

	// pretend the remote is awful
	time.Sleep(time.Second * 10)

	l.Warnf("finished submitting %d events", len(events))

	return nil
}

func NewPrehogSubmitter(ctx context.Context) usageSubmitFunc {
	client := prehogclient.NewTeleportReportingServiceClient(http.DefaultClient, "https://localhost:1234")

	return func(reporter *UsageReporter, events []*prehogapi.SubmitEventRequest) error {
		// Note: the backend doesn't support batching at the moment.
		for _, event := range events {
			// Note: this results in retrying the entire batch, which probably
			// isn't ideal.
			req := connect.NewRequest(event)
			if _, err := client.SubmitEvent(ctx, req); err != nil {
				return trace.Wrap(err)
			}
		}

		return nil
	}
}

func NewUsageReporter(ctx context.Context, clusterName types.ClusterName) (*UsageReporter, error) {
	l := log.WithFields(log.Fields{
		trace.Component: teleport.Component(teleport.ComponentUsageReporting),
	})

	// TODO: verify clusterName and clusterID. Do we send name or ID?
	anonymizer, err := utils.NewHMACAnonymizer(clusterName.GetClusterID())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	ctx, cancel := context.WithCancel(ctx)
	return &UsageReporter{
		Entry:           l,
		ctx:             ctx,
		cancel:          cancel,
		anonymizer:      anonymizer,
		events:          make(chan []*prehogapi.SubmitEventRequest),
		submissionQueue: make(chan []*prehogapi.SubmitEventRequest),
		submit:          dummySubmit, // TODO: pending real impl
		clock:           clockwork.NewRealClock(),
		clusterName:     clusterName,
		minBatchSize:    usageReporterMinBatchSize,
		maxBatchSize:    usageReporterMaxBatchSize,
		maxBatchAge:     usageReporterMaxBatchAge,
		maxBufferSize:   usageReporterMaxBufferSize,
	}, nil
}
