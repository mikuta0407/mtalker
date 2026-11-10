package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	botgateway "github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

func TestManagerCreateStoresSession(t *testing.T) {
	manager := NewManager()
	conn := &stubConn{guildID: snowflake.ID(1)}

	session, err := manager.Create(CreateParams{
		GuildID:        snowflake.ID(1),
		TextChannelID:  snowflake.ID(10),
		VoiceChannelID: snowflake.ID(20),
		Conn:           conn,
		QueueCapacity:  4,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if manager.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", manager.Len())
	}
	if session.GuildID() != snowflake.ID(1) {
		t.Fatalf("GuildID() = %d, want 1", session.GuildID())
	}
	if session.TextChannelID() != snowflake.ID(10) {
		t.Fatalf("TextChannelID() = %d, want 10", session.TextChannelID())
	}
	if session.VoiceChannelID() != snowflake.ID(20) {
		t.Fatalf("VoiceChannelID() = %d, want 20", session.VoiceChannelID())
	}
	if session.Conn() != conn {
		t.Fatalf("Conn() = %v, want %v", session.Conn(), conn)
	}
	if session.QueueCap() != 4 {
		t.Fatalf("QueueCap() = %d, want 4", session.QueueCap())
	}
	if _, ok := manager.Get(snowflake.ID(1)); !ok {
		t.Fatalf("Get() did not return stored session")
	}
}

func TestManagerRejectsDuplicateGuildSession(t *testing.T) {
	manager := NewManager()

	_, err := manager.Create(CreateParams{GuildID: snowflake.ID(1)})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err = manager.Create(CreateParams{GuildID: snowflake.ID(1)})
	if err == nil {
		t.Fatal("second Create() error = nil, want ErrSessionAlreadyExists")
	}
	if err != ErrSessionAlreadyExists {
		t.Fatalf("second Create() error = %v, want %v", err, ErrSessionAlreadyExists)
	}
}

func TestManagerSupportsMultipleGuildSessionsIndependently(t *testing.T) {
	manager := NewManager()
	session1, err := manager.Create(CreateParams{GuildID: snowflake.ID(1), QueueCapacity: 2})
	if err != nil {
		t.Fatalf("Create(session1) error = %v", err)
	}
	session2, err := manager.Create(CreateParams{GuildID: snowflake.ID(2), QueueCapacity: 2})
	if err != nil {
		t.Fatalf("Create(session2) error = %v", err)
	}

	if err := session1.Enqueue(PlaybackRequest{Content: "guild-1"}); err != nil {
		t.Fatalf("session1.Enqueue() error = %v", err)
	}
	if session1.QueueLen() != 1 {
		t.Fatalf("session1.QueueLen() = %d, want 1", session1.QueueLen())
	}
	if session2.QueueLen() != 0 {
		t.Fatalf("session2.QueueLen() = %d, want 0", session2.QueueLen())
	}

	tempDir := t.TempDir()
	file1 := filepath.Join(tempDir, "guild1.wav")
	file2 := filepath.Join(tempDir, "guild2.wav")
	if err := os.WriteFile(file1, []byte("g1"), 0o600); err != nil {
		t.Fatalf("WriteFile(file1) error = %v", err)
	}
	if err := os.WriteFile(file2, []byte("g2"), 0o600); err != nil {
		t.Fatalf("WriteFile(file2) error = %v", err)
	}
	session1.TrackTempFile(file1)
	session2.TrackTempFile(file2)

	session1.Close(context.Background())

	if manager.Len() != 1 {
		t.Fatalf("Len() after closing session1 = %d, want 1", manager.Len())
	}
	if _, ok := manager.Get(snowflake.ID(1)); ok {
		t.Fatal("session1 should be removed from manager")
	}
	if _, ok := manager.Get(snowflake.ID(2)); !ok {
		t.Fatal("session2 should remain in manager")
	}
	if _, err := os.Stat(file1); !os.IsNotExist(err) {
		t.Fatalf("file1 should be removed when session1 closes, stat err = %v", err)
	}
	if _, err := os.Stat(file2); err != nil {
		t.Fatalf("file2 should remain until session2 closes, stat err = %v", err)
	}

	session2.Close(context.Background())
	if manager.Len() != 0 {
		t.Fatalf("Len() after closing session2 = %d, want 0", manager.Len())
	}
	if _, err := os.Stat(file2); !os.IsNotExist(err) {
		t.Fatalf("file2 should be removed when session2 closes, stat err = %v", err)
	}
}

func TestManagerRemovesSessionWhenClosedDirectly(t *testing.T) {
	manager := NewManager()
	session, err := manager.Create(CreateParams{GuildID: snowflake.ID(1)})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	session.Close(context.Background())

	if manager.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", manager.Len())
	}
	if _, ok := manager.Get(snowflake.ID(1)); ok {
		t.Fatal("session should be removed after direct Close()")
	}
}

func TestManagerDetachRemovesSessionWithoutClosingIt(t *testing.T) {
	manager := NewManager()
	conn := &stubConn{guildID: snowflake.ID(1)}

	session, err := manager.Create(CreateParams{GuildID: snowflake.ID(1), Conn: conn})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	detached, err := manager.Detach(snowflake.ID(1))
	if err != nil {
		t.Fatalf("Detach() error = %v", err)
	}
	if detached != session {
		t.Fatalf("Detach() session = %v, want %v", detached, session)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", manager.Len())
	}
	if conn.closeCalls != 0 {
		t.Fatalf("closeCalls = %d, want 0", conn.closeCalls)
	}
	if session.Closed() {
		t.Fatal("session should remain open after Detach()")
	}

	session.Close(context.Background())
	if conn.closeCalls != 1 {
		t.Fatalf("closeCalls after Close() = %d, want 1", conn.closeCalls)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len() after Close() = %d, want 0", manager.Len())
	}
	if _, ok := manager.Get(snowflake.ID(1)); ok {
		t.Fatal("session should remain detached after Close()")
	}
}

func TestManagerDestroyClosesAndRemovesSession(t *testing.T) {
	manager := NewManager()
	conn := &stubConn{guildID: snowflake.ID(1)}

	session, err := manager.Create(CreateParams{
		GuildID: snowflake.ID(1),
		Conn:    conn,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := manager.Destroy(context.Background(), snowflake.ID(1)); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}

	if manager.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", manager.Len())
	}
	if !session.Closed() {
		t.Fatal("Closed() = false, want true")
	}
	if conn.closeCalls != 1 {
		t.Fatalf("closeCalls = %d, want 1", conn.closeCalls)
	}
	if err := session.Enqueue(PlaybackRequest{Content: "hello"}); err != ErrSessionClosed {
		t.Fatalf("Enqueue() error = %v, want %v", err, ErrSessionClosed)
	}
	select {
	case <-session.Context().Done():
	default:
		t.Fatal("session context is not canceled")
	}
}

func TestManagerDestroyRecoversWhenConnClosePanics(t *testing.T) {
	manager := NewManager()
	conn := &stubConn{guildID: snowflake.ID(1), panicOnClose: true}

	session, err := manager.Create(CreateParams{
		GuildID: snowflake.ID(1),
		Conn:    conn,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := manager.Destroy(context.Background(), snowflake.ID(1)); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}

	if manager.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", manager.Len())
	}
	if !session.Closed() {
		t.Fatal("Closed() = false, want true")
	}
	if conn.closeCalls != 1 {
		t.Fatalf("closeCalls = %d, want 1", conn.closeCalls)
	}
	select {
	case <-session.Context().Done():
	default:
		t.Fatal("session context is not canceled")
	}
}

func TestManagerCloseClosesAllSessions(t *testing.T) {
	manager := NewManager()
	conn1 := &stubConn{guildID: snowflake.ID(1)}
	conn2 := &stubConn{guildID: snowflake.ID(2)}

	if _, err := manager.Create(CreateParams{GuildID: snowflake.ID(1), Conn: conn1}); err != nil {
		t.Fatalf("Create(session1) error = %v", err)
	}
	if _, err := manager.Create(CreateParams{GuildID: snowflake.ID(2), Conn: conn2}); err != nil {
		t.Fatalf("Create(session2) error = %v", err)
	}

	manager.Close(context.Background())

	if manager.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", manager.Len())
	}
	if conn1.closeCalls != 1 || conn2.closeCalls != 1 {
		t.Fatalf("closeCalls = (%d, %d), want (1, 1)", conn1.closeCalls, conn2.closeCalls)
	}
}

type stubConn struct {
	guildID      snowflake.ID
	channelID    *snowflake.ID
	closeCalls   int
	panicOnClose bool
}

func (c *stubConn) Gateway() voice.Gateway {
	return nil
}

func (c *stubConn) UDP() voice.UDPConn {
	return nil
}

func (c *stubConn) ChannelID() *snowflake.ID {
	return c.channelID
}

func (c *stubConn) GuildID() snowflake.ID {
	return c.guildID
}

func (c *stubConn) UserIDBySSRC(ssrc uint32) snowflake.ID {
	return 0
}

func (c *stubConn) SetSpeaking(ctx context.Context, flags voice.SpeakingFlags) error {
	return nil
}

func (c *stubConn) SetOpusFrameProvider(handler voice.OpusFrameProvider) {}

func (c *stubConn) SetOpusFrameReceiver(handler voice.OpusFrameReceiver) {}

func (c *stubConn) Open(ctx context.Context, channelID snowflake.ID, selfMute bool, selfDeaf bool) error {
	c.channelID = &channelID
	return nil
}

func (c *stubConn) Close(ctx context.Context) {
	c.closeCalls++
	if c.panicOnClose {
		panic("stub close panic")
	}
	channelID := snowflake.ID(0)
	c.channelID = &channelID
}

func (c *stubConn) HandleVoiceStateUpdate(update botgateway.EventVoiceStateUpdate) {}

func (c *stubConn) HandleVoiceServerUpdate(update botgateway.EventVoiceServerUpdate) {}

func (c *stubConn) DAVE() godave.Session { return nil }
