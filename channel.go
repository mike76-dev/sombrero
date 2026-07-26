package main

// channel represents a cross-session connection channel in SMB2.
// It is used to support multi-channel connections, which allow
// multiple network connections to be used for a single SMB2 session.
type channel struct {
	signingKey []byte
	connection *connection
}
