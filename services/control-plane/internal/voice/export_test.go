package voice

// Export unexported methods and types for testing.

// HandleJoined exposes handleJoined for testing.
func (s *NATSSubscriber) HandleJoined(data []byte) { s.handleJoined(data) }

// HandleLeft exposes handleLeft for testing.
func (s *NATSSubscriber) HandleLeft(data []byte) { s.handleLeft(data) }

// HandleRoomEmpty exposes handleRoomEmpty for testing.
func (s *NATSSubscriber) HandleRoomEmpty(data []byte) { s.handleRoomEmpty(data) }

// HandleHeartbeat exposes handleHeartbeat for testing.
func (s *NATSSubscriber) HandleHeartbeat(data []byte) { s.handleHeartbeat(data) }

// TestRoomContext is an exported wrapper around roomContext for testing.
type TestRoomContext struct {
	IsDM     bool
	ServerID string
}

// ResolveRoom exposes resolveRoom for testing and returns a TestRoomContext.
func (s *NATSSubscriber) ResolveRoom(channelID string) (*TestRoomContext, error) {
	ctx, err := s.resolveRoom(channelID)
	if err != nil {
		return nil, err
	}
	return &TestRoomContext{
		IsDM:     ctx.isDM,
		ServerID: ctx.serverID,
	}, nil
}
