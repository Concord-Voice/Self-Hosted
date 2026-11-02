package websocket

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capture the pre-snapshot nil result before notifying the test. Canceling only
// this event parent leaves the socket/Hub lifetime live, so those guards cannot
// mask a missing parent recheck after the deadline mutex becomes available.
type lifecycleAuthoritySnapshotContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (ctx *lifecycleAuthoritySnapshotContext) Err() error {
	err := ctx.Context.Err()
	if err == nil {
		ctx.once.Do(func() { close(ctx.checked) })
	}
	return err
}
func TestEventAdmissionLifecycleAuthoritySnapshotRechecksParentAfterDeadlineWait(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel%t", canceled), func(t *testing.T) {
			hub := lifecycleHub(t)
			client := lifecycleClient(t, hub)
			parent, cancel := context.WithCancel(client.ingressCtx)
			defer cancel()
			ctx := &lifecycleAuthoritySnapshotContext{Context: parent, checked: make(chan struct{})}
			client.deadlineMu.Lock()
			var unlockOnce sync.Once
			t.Cleanup(func() { unlockOnce.Do(client.deadlineMu.Unlock) })
			result := make(chan bool, 1)
			go func() { result <- client.ingressAuthorityLive(ctx) }()
			lifecycleAwait(t, ctx.checked, "authority passes initial cancellation snapshot")
			if canceled {
				cancel()
			}
			unlockOnce.Do(client.deadlineMu.Unlock)
			select {
			case live := <-result:
				assert.Equal(t, !canceled, live)
			case <-time.After(time.Second):
				t.Fatal("authority deadline snapshot did not settle")
			}
			lifecycleRequireActive(client.ingressCtx, t)
			lifecycleRequireActive(hub.ingressCtx, t)
			require.True(t, time.Now().Before(client.tokenDeadline()), "fixture authority deadline must remain live")
		})
	}
}
