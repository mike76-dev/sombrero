package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
)

// readMessage reads a new SMB message from the TCP connection.
// An SMB message is prepended with a 4-byte header, which encodes the length of the message.
func readMessage(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 4)
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		return nil, fmt.Errorf("error reading TCP header: %w", err)
	}
	if n < 4 {
		return nil, fmt.Errorf("supposed to read 4 bytes but got %d", n)
	}
	if buf[0] != 0 {
		return nil, errors.New("first byte is supposed to be zero")
	}

	length := binary.BigEndian.Uint32(buf)
	msg := make([]byte, length)

	n, err = io.ReadFull(conn, msg)
	if err != nil {
		return nil, fmt.Errorf("error reading message: %w", err)
	}
	if n < int(length) {
		return nil, fmt.Errorf("supposed to read %d bytes but got %d", length, n)
	}

	return msg, nil
}

// writeMessage writes the SMB message to the underlying TCP connection.
// An SMB message is prepended with a 4-byte header, which encodes the length of the message.
func writeMessage(conn net.Conn, msg []byte) error {
	length := uint32(len(msg))
	if length >= uint32(math.Pow(2, 24)) {
		return errors.New("message too long")
	}

	msg = append(make([]byte, 4), msg...)
	binary.BigEndian.PutUint32(msg[:4], length)

	n, err := conn.Write(msg)
	if err != nil {
		return fmt.Errorf("error writing message: %v", err)
	}
	if n < int(length)+4 {
		return fmt.Errorf("supposed to write %d bytes but got %d", length+4, n)
	}

	return nil
}
