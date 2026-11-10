package appbot

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	botgateway "github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/mikuta0407/mtalker/internal/session"
)

func TestPrimeVoiceConnectionSendsSilenceFrame(t *testing.T) {
	handler := &Handler{}
	conn := &stubVoiceConn{udpConn: &stubVoiceUDPConn{}}
	sess := session.New(session.CreateParams{Conn: conn})

	if err := handler.primeVoiceConnection(sess); err != nil {
		t.Fatalf("primeVoiceConnection() error = %v", err)
	}

	if len(conn.speakingFlags) != 1 {
		t.Fatalf("speaking calls = %d, want 1", len(conn.speakingFlags))
	}
	if conn.speakingFlags[0] != voice.SpeakingFlagMicrophone {
		t.Fatalf("first speaking flag = %v, want %v", conn.speakingFlags[0], voice.SpeakingFlagMicrophone)
	}
	if len(conn.udpConn.writes) != 1 {
		t.Fatalf("udp writes = %d, want 1", len(conn.udpConn.writes))
	}
	if !bytes.Equal(conn.udpConn.writes[0], voice.SilenceAudioFrame) {
		t.Fatalf("first udp write = %v, want silence frame %v", conn.udpConn.writes[0], voice.SilenceAudioFrame)
	}
}

func TestSendIdleSilenceFrameSendsSingleSilenceFrame(t *testing.T) {
	handler := &Handler{}
	conn := &stubVoiceConn{udpConn: &stubVoiceUDPConn{}}
	sess := session.New(session.CreateParams{Conn: conn})

	if err := handler.sendIdleSilenceFrame(sess, conn); err != nil {
		t.Fatalf("sendIdleSilenceFrame() error = %v", err)
	}

	if len(conn.speakingFlags) != 1 {
		t.Fatalf("speaking calls = %d, want 1", len(conn.speakingFlags))
	}
	if len(conn.udpConn.writes) != 1 {
		t.Fatalf("udp writes = %d, want 1", len(conn.udpConn.writes))
	}
	if !bytes.Equal(conn.udpConn.writes[0], voice.SilenceAudioFrame) {
		t.Fatalf("first udp write = %v, want silence frame %v", conn.udpConn.writes[0], voice.SilenceAudioFrame)
	}
}

func TestSendIdleSilenceFrameSkipsSpeakingWhenAlreadyActive(t *testing.T) {
	handler := &Handler{}
	conn := &stubVoiceConn{udpConn: &stubVoiceUDPConn{}}
	sess := session.New(session.CreateParams{Conn: conn})
	sess.SetIdleSpeakingActive(true)

	if err := handler.sendIdleSilenceFrame(sess, conn); err != nil {
		t.Fatalf("sendIdleSilenceFrame() error = %v", err)
	}

	if len(conn.speakingFlags) != 0 {
		t.Fatalf("speaking calls = %d, want 0", len(conn.speakingFlags))
	}
	if len(conn.udpConn.writes) != 1 {
		t.Fatalf("udp writes = %d, want 1", len(conn.udpConn.writes))
	}
}

func TestWaitForVoiceServerRecoveryCallsFallbackAfterGracePeriod(t *testing.T) {
	handler := newTestHandlerWithSession(t, 1)
	handler.voiceServerUpdateGracePeriod = 20 * time.Millisecond

	fallbackCalled := make(chan struct{}, 1)
	handler.WaitForVoiceServerRecovery(1, func() {
		fallbackCalled <- struct{}{}
	})

	select {
	case <-fallbackCalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("fallback was not called after grace period")
	}
}

func TestOnVoiceServerUpdateKeepsPendingVoiceGatewayCloseUntilGatewayReady(t *testing.T) {
	handler := newTestHandlerWithSession(t, 1)
	handler.voiceServerUpdateGracePeriod = 100 * time.Millisecond

	fallbackCalled := make(chan struct{}, 1)
	handler.WaitForVoiceServerRecovery(1, func() {
		fallbackCalled <- struct{}{}
	})

	endpoint := "voice.example.test"
	handler.OnVoiceServerUpdate(&events.VoiceServerUpdate{
		EventVoiceServerUpdate: gateway.EventVoiceServerUpdate{
			GuildID:  1,
			Endpoint: &endpoint,
		},
	})

	select {
	case <-fallbackCalled:
		t.Fatal("fallback should remain pending until the voice gateway is ready")
	case <-time.After(50 * time.Millisecond):
	}

	if !handler.hasPendingVoiceGatewayClose(1) {
		t.Fatal("pending recovery should remain until gateway ready")
	}

	handler.onVoiceGatewayEvent(1, voice.OpcodeSessionDescription)

	select {
	case <-fallbackCalled:
		t.Fatal("fallback should not be called after gateway recovery")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestOnVoiceServerUpdateExtendsRecoveryDeadline(t *testing.T) {
	handler := newTestHandlerWithSession(t, 1)
	handler.voiceServerUpdateGracePeriod = 80 * time.Millisecond

	fallbackCalled := make(chan struct{}, 1)
	handler.WaitForVoiceServerRecovery(1, func() {
		fallbackCalled <- struct{}{}
	})

	time.Sleep(50 * time.Millisecond)

	endpoint := "voice.example.test"
	handler.OnVoiceServerUpdate(&events.VoiceServerUpdate{
		EventVoiceServerUpdate: gateway.EventVoiceServerUpdate{
			GuildID:  1,
			Endpoint: &endpoint,
		},
	})

	select {
	case <-fallbackCalled:
		t.Fatal("fallback should be extended after voice server update")
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case <-fallbackCalled:
	case <-time.After(120 * time.Millisecond):
		t.Fatal("fallback should fire after the extended recovery deadline")
	}
}

func TestProcessTTSDisconnectDestroysSessionAndReturnsSuccessMessage(t *testing.T) {
	handler := &Handler{sessions: session.NewManager()}
	conn := &stubVoiceConn{udpConn: &stubVoiceUDPConn{}}

	if _, err := handler.sessions.Create(session.CreateParams{
		GuildID:        1,
		TextChannelID:  10,
		VoiceChannelID: 20,
		Conn:           conn,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var got string
	handler.processTTSDisconnect(1, 10, 100, func(content string) {
		got = content
	})

	if got != "読み上げセッションを切断しました。" {
		t.Fatalf("final response = %q, want %q", got, "読み上げセッションを切断しました。")
	}
	if handler.sessions.Exists(1) {
		t.Fatal("session should be removed after disconnect")
	}
	if conn.closeCalls != 1 {
		t.Fatalf("closeCalls = %d, want 1", conn.closeCalls)
	}
}

func TestProcessTTSDisconnectReturnsAlreadyClosedMessageWhenSessionMissing(t *testing.T) {
	handler := &Handler{sessions: session.NewManager()}

	var got string
	handler.processTTSDisconnect(1, 10, 100, func(content string) {
		got = content
	})

	if got != "読み上げセッションは既に切断されています。" {
		t.Fatalf("final response = %q, want %q", got, "読み上げセッションは既に切断されています。")
	}
}

func newTestHandlerWithSession(t *testing.T, guildID snowflake.ID) *Handler {
	t.Helper()

	handler := &Handler{
		sessions:               session.NewManager(),
		voiceGatewayRecoveries: make(map[snowflake.ID]*voiceGatewayRecovery),
	}
	if _, err := handler.sessions.Create(session.CreateParams{
		GuildID:        guildID,
		TextChannelID:  10,
		VoiceChannelID: 20,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return handler
}

type stubVoiceConn struct {
	udpConn       *stubVoiceUDPConn
	speakingFlags []voice.SpeakingFlags
	closeCalls    int
}

func (c *stubVoiceConn) Gateway() voice.Gateway { return nil }
func (c *stubVoiceConn) UDP() voice.UDPConn     { return c.udpConn }
func (c *stubVoiceConn) ChannelID() *snowflake.ID {
	return nil
}
func (c *stubVoiceConn) GuildID() snowflake.ID { return 0 }
func (c *stubVoiceConn) UserIDBySSRC(ssrc uint32) snowflake.ID {
	return 0
}
func (c *stubVoiceConn) SetSpeaking(ctx context.Context, flags voice.SpeakingFlags) error {
	c.speakingFlags = append(c.speakingFlags, flags)
	return nil
}
func (c *stubVoiceConn) SetOpusFrameProvider(handler voice.OpusFrameProvider) {}
func (c *stubVoiceConn) SetOpusFrameReceiver(handler voice.OpusFrameReceiver) {}
func (c *stubVoiceConn) Open(ctx context.Context, channelID snowflake.ID, selfMute bool, selfDeaf bool) error {
	return nil
}
func (c *stubVoiceConn) Close(ctx context.Context)                                        { c.closeCalls++ }
func (c *stubVoiceConn) HandleVoiceStateUpdate(update botgateway.EventVoiceStateUpdate)   {}
func (c *stubVoiceConn) HandleVoiceServerUpdate(update botgateway.EventVoiceServerUpdate) {}

type stubVoiceUDPConn struct {
	writes [][]byte
}

func (c *stubVoiceUDPConn) LocalAddr() net.Addr  { return nil }
func (c *stubVoiceUDPConn) RemoteAddr() net.Addr { return nil }
func (c *stubVoiceUDPConn) SetSecretKey(mode voice.EncryptionMode, secretKey []byte) error {
	return nil
}
func (c *stubVoiceUDPConn) SetDeadline(t time.Time) error      { return nil }
func (c *stubVoiceUDPConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *stubVoiceUDPConn) SetWriteDeadline(t time.Time) error { return nil }
func (c *stubVoiceUDPConn) Open(ctx context.Context, ip string, port int, ssrc uint32) (string, int, error) {
	return "", 0, nil
}
func (c *stubVoiceUDPConn) Close() error                       { return nil }
func (c *stubVoiceUDPConn) Read(p []byte) (int, error)         { return 0, io.EOF }
func (c *stubVoiceUDPConn) ReadPacket() (*voice.Packet, error) { return nil, io.EOF }
func (c *stubVoiceUDPConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (c *stubVoiceConn) DAVE() godave.Session { return nil }
