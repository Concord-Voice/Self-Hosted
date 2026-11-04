package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
	"github.com/lib/pq"
)

// Gate a real PostgreSQL Ping at its entry so cancellation is deterministic,
// rather than hoping a one-second owner deadline overlaps the watch ticker.
type gatedServePingConnector struct {
	driver.Connector
	entered chan context.Context
	proceed <-chan struct{}
	result  chan error
}

func (c gatedServePingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return gatedServePingConn{Conn: conn, gate: c}, nil
}

type gatedServePingConn struct {
	driver.Conn
	gate gatedServePingConnector
}

func (c gatedServePingConn) Ping(ctx context.Context) error {
	c.gate.entered <- ctx
	<-c.gate.proceed
	err := c.Conn.(driver.Pinger).Ping(ctx)
	c.gate.result <- err
	return err
}

func TestServeLockWatchCancellationPreservesLivePing(t *testing.T) {
	for _, mode := range []string{"parent cancellation", "stop"} {
		t.Run(mode, func(t *testing.T) {
			f := authoritydb.Setup(t)
			base, err := pq.NewConnector(authoritydb.URL(t, authoritydb.EnvURL, authoritydb.DBName))
			if err != nil {
				t.Fatal(err)
			}
			proceed := make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(proceed) }) }
			gate := gatedServePingConnector{Connector: base, entered: make(chan context.Context, 1), proceed: proceed, result: make(chan error, 1)}
			db := sql.OpenDB(gate)
			defer func() { _ = db.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lock, err := AcquireServeLock(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			pid := backendPID(ctx, t, lock.conn)
			watched, stop := lock.Watch(ctx)
			defer stop()
			defer resume()
			var pingCtx context.Context
			select {
			case pingCtx = <-gate.entered:
			case <-time.After(ServeLockWatchInterval + time.Second):
				t.Fatal("watch did not start its ping")
			}
			deadline, ok := pingCtx.Deadline()
			if !ok || time.Until(deadline) > RolePollInterval {
				t.Fatal("ping has no bounded timeout")
			}
			stopped := make(chan struct{})
			if mode == "parent cancellation" {
				cancel()
			}
			go func() { stop(); close(stopped) }()
			select {
			case <-watched.Done():
			case <-time.After(time.Second):
				t.Fatal("work cancellation did not propagate")
			}
			if err := pingCtx.Err(); err != nil {
				t.Fatalf("ordinary work cancellation interrupted the lock session's live ping: %v", err)
			}
			select {
			case <-stopped:
				t.Fatal("stop returned while the ping still owned the connection")
			default:
			}
			resume()
			select {
			case <-stopped:
			case <-time.After(RolePollInterval + time.Second):
				t.Fatal("stop did not join the bounded ping")
			}
			if err := <-gate.result; err != nil {
				t.Fatalf("live PostgreSQL ping: %v", err)
			}
			// A successful unlock must make the lock immediately acquirable, without
			// waiting for asynchronous PostgreSQL backend teardown.
			lock.Release()
			next, err := AcquireServeLock(context.Background(), f.Owner)
			if err != nil {
				t.Fatalf("acquire immediately after cancelled watch and release: %v", err)
			}
			next.Release()
			waitSessionGone(context.Background(), t, f.Super, pid)
		})
	}
}
