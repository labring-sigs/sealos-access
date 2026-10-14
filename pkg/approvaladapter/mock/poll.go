/*
Copyright 2026 labring.

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

package mock

import (
	"context"
	"log"
	"time"

	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
)

// defaultPollInterval is the sleep between poll cycles.
const defaultPollInterval = 30 * time.Second

// Poll runs a simple polling loop that processes approval instances as they
// become available on the knownInstanceCodes channel.
//
// This function demonstrates the pattern that a real ApprovalSource would
// follow. In production, the polling logic (webhook listener, event queue
// consumer, or periodic poll) is entirely the responsibility of the
// ApprovalSource implementation.
//
// The loop exits when ctx is cancelled. It is safe to call concurrently.
func Poll(ctx context.Context, source approvaladapter.ApprovalSource, approver approvaladapter.Approver, notifier approvaladapter.Notifier, interval time.Duration, knownInstanceCodes <-chan string) {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	log.Printf("mock poll: starting with interval %s", interval)
	for {
		select {
		case <-ctx.Done():
			log.Printf("mock poll: stopped: %v", ctx.Err())
			return
		case code, ok := <-knownInstanceCodes:
			if !ok {
				return
			}
			if err := processApproval(ctx, source, approver, notifier, code); err != nil {
				log.Printf("mock poll: process %s: %v", code, err)
			}
		case <-time.After(interval):
			// In a real implementation you would list pending approvals
			// from the external system here. For the mock, we just wait
			// for codes to arrive on the channel.
		}
	}
}

// processApproval fetches an approval instance, submits it to the Broker,
// and notifies the target user. This is the core pipeline that a real
// ApprovalSource would execute for each approved event.
func processApproval(ctx context.Context, source approvaladapter.ApprovalSource, approver approvaladapter.Approver, notifier approvaladapter.Notifier, instanceCode string) error {
	pipeline, err := approvaladapter.NewPipeline(source, approver, notifier)
	if err != nil {
		return err
	}
	if err := pipeline.Process(ctx, instanceCode); err != nil {
		return err
	}
	log.Printf("mock poll: processed %s", instanceCode)
	return nil
}
