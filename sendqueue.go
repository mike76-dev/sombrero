package main

import (
	"sync"

	"github.com/mike76-dev/sombrero/smb2"
)

// sendQueue holds a connection's outgoing messages in two lanes, small ones sent before file data.
// Pushing never blocks; its length is bounded by the requests the client's credits allow.
type sendQueue struct {
	mu      sync.Mutex
	control [][]byte
	bulk    [][]byte
	ready   chan struct{} // signalled on push, holds one signal
}

func newSendQueue() *sendQueue {
	return &sendQueue{ready: make(chan struct{}, 1)}
}

// push queues a message, in the file-data lane if bulk is set.
func (q *sendQueue) push(msg []byte, bulk bool) {
	q.mu.Lock()
	if bulk {
		q.bulk = append(q.bulk, msg)
	} else {
		q.control = append(q.control, msg)
	}
	q.mu.Unlock()

	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// pop returns the next message to send, or nil if there is none.
func (q *sendQueue) pop() []byte {
	q.mu.Lock()
	defer q.mu.Unlock()

	return popFront(&q.control, &q.bulk)
}

// popFront takes the first message of the first non-empty lane.
func popFront(lanes ...*[][]byte) []byte {
	for _, lane := range lanes {
		if len(*lane) == 0 {
			continue
		}

		msg := (*lane)[0]
		(*lane)[0] = nil
		*lane = (*lane)[1:]
		if len(*lane) == 0 {
			*lane = nil // release the backing array
		}
		return msg
	}

	return nil
}

// carriesFileData reports whether a response belongs in the file-data lane: a successful read.
func carriesFileData(resp smb2.GenericResponse) bool {
	h := resp.Header()
	return h.Command() == smb2.SMB2_READ && h.Status() == smb2.STATUS_OK
}

// drainSendQueue passes queued messages to deliver, small ones first, until the connection closes
// or deliver fails.
func (c *connection) drainSendQueue(deliver func([]byte) error) error {
	for {
		if msg := c.sendQueue.pop(); msg != nil {
			if err := deliver(msg); err != nil {
				return err
			}
			continue
		}

		select {
		case <-c.closeChan:
			return nil
		case <-c.sendQueue.ready:
		}
	}
}
