package main

// channel represents a cross-session connection channel in SMB2.
// It is used to support multi-channel connections, which allow
// multiple network connections to be used for a single SMB2 session.
type channel struct {
	signingKey []byte
	connection *connection
}

// bindChannel inserts the connection into the channel list of the session, or updates
// the signing key of the channel if the connection is already bound to the session.
// The key of a channel is derived from the authentication that established it: for the
// connection the session was created on, it is the signing key of the session itself.
func (ss *session) bindChannel(c *connection, signingKey []byte) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ch, found := ss.channelList[c.clientName]
	if !found || ch.connection != c {
		ch = &channel{connection: c}
		ss.channelList[c.clientName] = ch
	}

	ch.signingKey = signingKey
}

// unbindChannel removes the connection from the channel list of the session.
func (ss *session) unbindChannel(c *connection) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ch, found := ss.channelList[c.clientName]; found && ch.connection == c {
		delete(ss.channelList, c.clientName)
	}
}

// channel looks up the channel of the session that the given connection belongs to.
// It returns nil if the connection isn't bound to the session.
func (ss *session) channel(c *connection) *channel {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	// The channel list is keyed by the name of the connection, but a name may be reused
	// by a later connection, so the connection itself decides whether the channel matches.
	ch, found := ss.channelList[c.clientName]
	if !found || ch.connection != c {
		return nil
	}

	return ch
}
