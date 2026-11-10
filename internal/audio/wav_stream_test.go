package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	botgateway "github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
)

func TestWAVStreamNextOpusFrame(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "test.wav")
	writePCM16WAV(t, filePath, 24000, 1, generateSamples(24000/10))

	stream, err := NewWAVStream(filePath)
	if err != nil {
		t.Fatalf("NewWAVStream() error = %v", err)
	}
	defer stream.Close()

	frames := 0
	for {
		frame, err := stream.NextOpusFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("NextOpusFrame() error = %v", err)
		}
		if len(frame) == 0 {
			t.Fatal("NextOpusFrame() returned empty frame")
		}
		frames++
	}

	if frames == 0 {
		t.Fatal("expected at least one opus frame")
	}
}

func TestNewWAVStreamFromReader(t *testing.T) {
	wavData := buildPCM16WAV(t, 24000, 1, generateSamples(24000/10))

	stream, err := NewWAVStreamFromReader(bytes.NewReader(wavData))
	if err != nil {
		t.Fatalf("NewWAVStreamFromReader() error = %v", err)
	}
	defer stream.Close()

	frame, err := stream.NextOpusFrame()
	if err != nil {
		t.Fatalf("NextOpusFrame() error = %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("NextOpusFrame() returned empty frame")
	}
}

func TestSendOpusFrameStreamPlaysWAVAndTogglesSpeaking(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "test.wav")
	writePCM16WAV(t, filePath, DiscordSampleRate, 1, generateSamples(voice.OpusFrameSize))

	stream, err := NewWAVStream(filePath)
	if err != nil {
		t.Fatalf("NewWAVStream() error = %v", err)
	}

	conn := &testConn{udpConn: &testUDPConn{}}
	if err := SendOpusFrameStream(context.Background(), conn, stream); err != nil {
		t.Fatalf("SendOpusFrameStream() error = %v", err)
	}

	if len(conn.speakingFlags) < 2 {
		t.Fatalf("speaking calls = %d, want at least 2", len(conn.speakingFlags))
	}
	if conn.speakingFlags[0] != voice.SpeakingFlagMicrophone {
		t.Fatalf("first speaking flag = %v, want %v", conn.speakingFlags[0], voice.SpeakingFlagMicrophone)
	}
	if conn.speakingFlags[len(conn.speakingFlags)-1] != voice.SpeakingFlagNone {
		t.Fatalf("last speaking flag = %v, want %v", conn.speakingFlags[len(conn.speakingFlags)-1], voice.SpeakingFlagNone)
	}
	if conn.udpConn.writeCount == 0 {
		t.Fatal("expected UDP writes during playback")
	}
}

func writePCM16WAV(t *testing.T, path string, sampleRate int, channels int, samples []int16) {
	t.Helper()

	if err := os.WriteFile(path, buildPCM16WAV(t, sampleRate, channels, samples), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func buildPCM16WAV(t *testing.T, sampleRate int, channels int, samples []int16) []byte {
	t.Helper()

	data := new(bytes.Buffer)
	for _, sample := range samples {
		if err := binary.Write(data, binary.LittleEndian, sample); err != nil {
			t.Fatalf("binary.Write(sample) error = %v", err)
		}
		if channels == 2 {
			if err := binary.Write(data, binary.LittleEndian, sample); err != nil {
				t.Fatalf("binary.Write(stereo sample) error = %v", err)
			}
		}
	}

	blockAlign := uint16(channels * BytesPerSample)
	byteRate := uint32(sampleRate) * uint32(blockAlign)
	dataSize := uint32(data.Len())

	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36)+dataSize)
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(pcmFormat))
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, byteRate)
	_ = binary.Write(buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, dataSize)
	buf.Write(data.Bytes())

	return buf.Bytes()
}

func generateSamples(length int) []int16 {
	samples := make([]int16, length)
	for i := range samples {
		samples[i] = int16((i % 128) * 128)
	}
	return samples
}

type testConn struct {
	udpConn       *testUDPConn
	speakingFlags []voice.SpeakingFlags
}

func (c *testConn) Gateway() voice.Gateway { return nil }
func (c *testConn) UDP() voice.UDPConn     { return c.udpConn }
func (c *testConn) ChannelID() *snowflake.ID {
	return nil
}
func (c *testConn) GuildID() snowflake.ID { return 0 }
func (c *testConn) UserIDBySSRC(ssrc uint32) snowflake.ID {
	return 0
}
func (c *testConn) SetSpeaking(ctx context.Context, flags voice.SpeakingFlags) error {
	c.speakingFlags = append(c.speakingFlags, flags)
	return nil
}
func (c *testConn) SetOpusFrameProvider(handler voice.OpusFrameProvider) {}
func (c *testConn) SetOpusFrameReceiver(handler voice.OpusFrameReceiver) {}
func (c *testConn) Open(ctx context.Context, channelID snowflake.ID, selfMute bool, selfDeaf bool) error {
	return nil
}
func (c *testConn) Close(ctx context.Context)                                        {}
func (c *testConn) HandleVoiceStateUpdate(update botgateway.EventVoiceStateUpdate)   {}
func (c *testConn) HandleVoiceServerUpdate(update botgateway.EventVoiceServerUpdate) {}

type testUDPConn struct{ writeCount int }

func (c *testUDPConn) LocalAddr() net.Addr  { return nil }
func (c *testUDPConn) RemoteAddr() net.Addr { return nil }
func (c *testUDPConn) SetSecretKey(mode voice.EncryptionMode, secretKey []byte) error {
	return nil
}
func (c *testUDPConn) SetDeadline(t time.Time) error      { return nil }
func (c *testUDPConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *testUDPConn) SetWriteDeadline(t time.Time) error { return nil }
func (c *testUDPConn) Open(ctx context.Context, ip string, port int, ssrc uint32) (string, int, error) {
	return "", 0, nil
}
func (c *testUDPConn) Close() error                       { return nil }
func (c *testUDPConn) Read(p []byte) (int, error)         { return 0, io.EOF }
func (c *testUDPConn) ReadPacket() (*voice.Packet, error) { return nil, io.EOF }
func (c *testUDPConn) Write(p []byte) (int, error) {
	c.writeCount++
	return len(p), nil
}

func (c *testConn) DAVE() godave.Session { return nil }
