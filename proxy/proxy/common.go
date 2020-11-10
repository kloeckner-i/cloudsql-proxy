// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package proxy implements client and server code for proxying an unsecure connection over SSL.
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/cloudsql-proxy/logging"
)

// SQLScope is the Google Cloud Platform scope required for executing API
// calls to Cloud SQL.
const SQLScope = "https://www.googleapis.com/auth/sqlservice.admin"

// ConnPacketStatus is used to track the last previous packet count.
type ConnPacketStatus map[*ConnSetEntry]uint64

type dbgConn struct {
	net.Conn
}

func (d dbgConn) Write(b []byte) (int, error) {
	x, y := d.Conn.Write(b)
	logging.Verbosef("write(%q) => (%v, %v)", b, x, y)
	return x, y
}

func (d dbgConn) Read(b []byte) (int, error) {
	x, y := d.Conn.Read(b)
	logging.Verbosef("read: (%v, %v) => %q", x, y, b[:x])
	return x, y
}

func (d dbgConn) Close() error {
	err := d.Conn.Close()
	logging.Verbosef("close: %v", err)
	return err
}

// myCopy is similar to io.Copy, but reports whether the returned error was due
// to a bad read or write. The returned error will never be nil
func myCopy(dst io.Writer, src io.Reader, packetCounter *uint64) (readErr bool, err error) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			atomic.AddUint64(packetCounter, 1)

			if _, werr := dst.Write(buf[:n]); werr != nil {
				if err == nil {
					return false, werr
				}
				// Read and write error; just report read error (it happened first).
				return true, err
			}
		}
		if err != nil {
			return true, err
		}
	}
}

func copyError(readDesc, writeDesc string, readErr bool, err error) {
	var desc string
	if readErr {
		desc = "Reading data from " + readDesc
	} else {
		desc = "Writing data to " + writeDesc
	}
	logging.Errorf("%v had error: %v", desc, err)
}

func copyThenClose(remote, local io.ReadWriteCloser, packetCounter *uint64, remoteDesc, localDesc string) {
	firstErr := make(chan error, 1)

	go func() {
		readErr, err := myCopy(remote, local, packetCounter)
		select {
		case firstErr <- err:
			if readErr && err == io.EOF {
				logging.Verbosef("Client closed %v", localDesc)
			} else {
				copyError(localDesc, remoteDesc, readErr, err)
			}
			remote.Close()
			local.Close()
		default:
		}
	}()

	readErr, err := myCopy(local, remote, packetCounter)
	select {
	case firstErr <- err:
		if readErr && err == io.EOF {
			logging.Verbosef("Instance %v closed connection", remoteDesc)
		} else {
			copyError(remoteDesc, localDesc, readErr, err)
		}
		remote.Close()
		local.Close()
	default:
		// In this case, the other goroutine exited first and already printed its
		// error (and closed the things).
	}
}

// NewConnSet initializes a new ConnSet and returns it.
func NewConnSet() *ConnSet {
	return &ConnSet{m: make(map[string][]*ConnSetEntry), packetCounts: make(ConnPacketStatus)}
}

// ConnSetEntry is a wrapper for a net.Conn stored in a ConnSet.
// It augments the net.Conn with an approximate packet counter.
// Which is used to track the activity of connections.
type ConnSetEntry struct {
	net.Conn
	packetCounter uint64
}

// A ConnSet tracks ConnSetEntrys associated with a provided ID.
// A nil ConnSet will be a no-op for all methods called on it.
type ConnSet struct {
	sync.RWMutex
	m            map[string][]*ConnSetEntry
	packetCounts ConnPacketStatus
}

// String returns a debug string for the ConnSet.
func (c *ConnSet) String() string {
	if c == nil {
		return "<nil>"
	}
	var b bytes.Buffer

	c.RLock()
	for id, conns := range c.m {
		fmt.Fprintf(&b, "ID %s:", id)
		for i, c := range conns {
			fmt.Fprintf(&b, "\n\t%d: %v", i, c)
		}
	}
	c.RUnlock()

	return b.String()
}

// Add saves the provided conn and associates it with the given string
// identifier.
func (c *ConnSet) Add(id string, conn *ConnSetEntry) {
	if c == nil {
		return
	}
	c.Lock()
	c.m[id] = append(c.m[id], conn)
	c.Unlock()
}

// IDs returns a slice of all identifiers which still have active connections.
func (c *ConnSet) IDs() []string {
	if c == nil {
		return nil
	}
	ret := make([]string, 0, len(c.m))

	c.RLock()
	for k := range c.m {
		ret = append(ret, k)
	}
	c.RUnlock()

	return ret
}

// Conns returns all active connections associated with the provided ids.
func (c *ConnSet) Conns(ids ...string) []*ConnSetEntry {
	if c == nil {
		return nil
	}
	var ret []*ConnSetEntry

	c.RLock()
	for _, id := range ids {
		ret = append(ret, c.m[id]...)
	}
	c.RUnlock()

	return ret
}

// Remove undoes an Add operation to have the set forget about a conn. Do not
// Remove an id/conn pair more than it has been Added.
func (c *ConnSet) Remove(id string, conn *ConnSetEntry) error {
	if c == nil {
		return nil
	}
	c.Lock()
	defer c.Unlock()

	pos := -1
	conns := c.m[id]
	for i, cc := range conns {
		if cc == conn {
			pos = i
			break
		}
	}

	if pos == -1 {
		return fmt.Errorf("couldn't find connection %v for id %s", conn, id)
	}

	if len(conns) == 1 {
		delete(c.m, id)
	} else {
		c.m[id] = append(conns[:pos], conns[pos+1:]...)
	}

	return nil
}

// Close closes every net.Conn contained in the set.
func (c *ConnSet) Close() error {
	if c == nil {
		return nil
	}
	var errs bytes.Buffer

	c.Lock()
	for id, conns := range c.m {
		for _, c := range conns {
			if err := c.Close(); err != nil {
				fmt.Fprintf(&errs, "%s close error: %v\n", id, err)
			}
		}
	}
	c.Unlock()

	if errs.Len() == 0 {
		return nil
	}

	return errors.New(errs.String())
}

// CloseIdle closes connections whose packet counts have not changed since the
// function was last invoked. This is used to detect idle connection suitable for
// draining.
func (c *ConnSet) CloseIdle() error {
	if c == nil {
		return nil
	}
	var errs bytes.Buffer

	c.Lock()
	defer c.Unlock()

	for id, conns := range c.m {
		for _, conn := range conns {
			curPacketCount := atomic.LoadUint64(&conn.packetCounter)
			if c.packetCounts[conn] == curPacketCount {
				if err := conn.Close(); err != nil {
					fmt.Fprintf(&errs, "%s close error: %v\n", id, err)
				}
			} else {
				c.packetCounts[conn] = curPacketCount
			}
		}
	}

	if errs.Len() == 0 {
		return nil
	}

	return errors.New(errs.String())
}
