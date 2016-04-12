package redisc

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/redisc/redistest"
	"github.com/garyburd/redigo/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test the conn.ReadOnly behaviour in a cluster setup with 1 replica per
// node. Runs multiple tests in the same function because setting up
// such a cluster is slow.
func TestConnReadOnlyWithReplicas(t *testing.T) {
	fn, ports := redistest.StartClusterWithReplicas(t, nil)
	defer fn()

	c := &Cluster{
		StartupNodes: []string{":" + ports[0]},
	}
	testWithReplicaClusterRefresh(t, c, ports)

	c = &Cluster{}
	testWithReplicaBindRandomWithoutNode(t, c)

	c = &Cluster{StartupNodes: []string{":" + ports[0]}}
	testWithReplicaBindEmptySlot(t, c)
}

func testWithReplicaBindEmptySlot(t *testing.T, c *Cluster) {
	conn := c.Get()
	defer conn.Close()

	// key "a" is not in node at [0], so will generate a refresh and connect
	// to a random node (to node at [0]).
	assert.NoError(t, conn.(*Conn).Bind("a"), "Bind to missing slot")
	if _, err := conn.Do("GET", "a"); assert.Error(t, err, "GET") {
		assert.Contains(t, err.Error(), "MOVED", "MOVED error")
	}

	// wait for refreshing to become false again
	c.mu.Lock()
	for c.refreshing {
		c.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		c.mu.Lock()
	}
	for i, v := range c.mapping {
		if !assert.NotEmpty(t, v, "Addr for %d", i) {
			break
		}
	}
	c.mu.Unlock()
}

func testWithReplicaBindRandomWithoutNode(t *testing.T, c *Cluster) {
	conn := c.Get()
	defer conn.Close()
	if err := conn.(*Conn).Bind(); assert.Error(t, err, "Bind fails") {
		assert.Contains(t, err.Error(), "failed to get a connection", "expected message")
	}
}

func testWithReplicaClusterRefresh(t *testing.T, c *Cluster, ports []string) {
	err := c.Refresh()
	if assert.NoError(t, err, "Refresh") {
		var prev string
		pix := -1
		for ix, node := range c.mapping {
			if assert.Equal(t, 2, len(node), "Mapping for slot %d must have 2 nodes", ix) {
				if node[0] != prev || ix == len(c.mapping)-1 {
					prev = node[0]
					t.Logf("%5d: %s\n", ix, node[0])
					pix++
				}
				if assert.NotEmpty(t, node[0]) {
					split0, split1 := strings.Index(node[0], ":"), strings.Index(node[1], ":")
					assert.Contains(t, ports, node[0][split0+1:], "expected address")
					assert.Contains(t, ports, node[1][split1+1:], "expected address")
				}
			} else {
				break
			}
		}
	}
}

func TestConnReadOnly(t *testing.T) {
	fn, ports := redistest.StartCluster(t, nil)
	defer fn()

	c := &Cluster{
		StartupNodes: []string{":" + ports[0]},
	}
	require.NoError(t, c.Refresh(), "Refresh")

	conn := c.Get()
	defer conn.Close()
	cc := conn.(*Conn)
	assert.NoError(t, cc.ReadOnly(), "ReadOnly")

	// both get and set work, because the connection is on a master
	_, err := cc.Do("SET", "b", 1)
	assert.NoError(t, err, "SET")
	v, err := redis.Int(cc.Do("GET", "b"))
	if assert.NoError(t, err, "GET") {
		assert.Equal(t, 1, v, "expected result")
	}

	conn2 := c.Get()
	defer conn2.Close()
	cc2 := conn2.(*Conn)
	assert.NoError(t, cc2.Bind(), "Bind")
	assert.Error(t, cc2.ReadOnly(), "ReadOnly after Bind")
}

func TestConnBind(t *testing.T) {
	fn, ports := redistest.StartCluster(t, nil)
	defer fn()

	for i, p := range ports {
		ports[i] = ":" + p
	}
	c := &Cluster{
		StartupNodes: ports,
		DialOptions:  []redis.DialOption{redis.DialConnectTimeout(2 * time.Second)},
	}
	require.NoError(t, c.Refresh(), "Refresh")

	conn := c.Get()
	defer conn.Close()

	if err := BindConn(conn, "A", "B"); assert.Error(t, err, "Bind with different keys") {
		assert.Contains(t, err.Error(), "keys do not belong to the same slot", "expected message")
	}
	assert.NoError(t, BindConn(conn, "A"), "Bind")
	if err := BindConn(conn, "B"); assert.Error(t, err, "Bind after Bind") {
		assert.Contains(t, err.Error(), "connection already bound", "expected message")
	}

	conn2 := c.Get()
	defer conn2.Close()

	assert.NoError(t, BindConn(conn2), "Bind without key")
}

func TestConnClose(t *testing.T) {
	c := &Cluster{
		StartupNodes: []string{":6379"},
	}
	conn := c.Get()
	require.NoError(t, conn.Close(), "Close")

	_, err := conn.Do("A")
	if assert.Error(t, err, "Do after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	if assert.Error(t, conn.Err(), "Err after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	if assert.Error(t, conn.Close(), "Close after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	if assert.Error(t, conn.Flush(), "Flush after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	if assert.Error(t, conn.Send("A"), "Send after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	_, err = conn.Receive()
	if assert.Error(t, err, "Receive after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	cc := conn.(*Conn)
	if assert.Error(t, cc.Bind("A"), "Bind after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
	if assert.Error(t, cc.ReadOnly(), "ReadOnly after Close") {
		assert.Contains(t, err.Error(), "redisc: closed", "expected message")
	}
}

func TestIsRedisError(t *testing.T) {
	err := error(redis.Error("CROSSSLOT some message"))
	assert.True(t, IsCrossSlot(err), "CrossSlot")
	assert.False(t, IsTryAgain(err), "CrossSlot")
	err = redis.Error("TRYAGAIN some message")
	assert.False(t, IsCrossSlot(err), "TryAgain")
	assert.True(t, IsTryAgain(err), "TryAgain")
	err = io.EOF
	assert.False(t, IsCrossSlot(err), "EOF")
	assert.False(t, IsTryAgain(err), "EOF")
	err = redis.Error("ERR some error")
	assert.False(t, IsCrossSlot(err), "ERR")
	assert.False(t, IsTryAgain(err), "ERR")
}
