package appbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
	"github.com/mikuta0407/mtalker/internal/audio"
	appconfig "github.com/mikuta0407/mtalker/internal/config"
	"github.com/mikuta0407/mtalker/internal/session"
	"github.com/mikuta0407/mtalker/internal/tts"
)

const voiceConnectTimeout = 30 * time.Second
const voiceCleanupTimeout = 2 * time.Second
const defaultVoiceServerUpdateGracePeriod = 5 * time.Second

type voiceGatewayRecovery struct {
	timer    *time.Timer
	fallback func()
}

type Handler struct {
	synthesizer tts.Synthesizer
	sessions    *session.Manager

	voiceGatewayCloseMu          sync.Mutex
	voiceGatewayRecoveries       map[snowflake.ID]*voiceGatewayRecovery
	voiceServerUpdateGracePeriod time.Duration
}

func NewHandler(cfg appconfig.Config) *Handler {
	return &Handler{
		synthesizer: tts.NewOpenJTalkSynthesizer(tts.OpenJTalkConfig{
			CommandPath:    cfg.OpenJTalkPath,
			DictionaryPath: cfg.DICPath,
			VoicePath:      cfg.VoicePath,
		}),
		sessions:                     session.NewManager(),
		voiceGatewayRecoveries:       make(map[snowflake.ID]*voiceGatewayRecovery),
		voiceServerUpdateGracePeriod: defaultVoiceServerUpdateGracePeriod,
	}
}

func (h *Handler) Sessions() *session.Manager {
	return h.sessions
}

func (h *Handler) Close(ctx context.Context) {
	h.cancelAllPendingVoiceGatewayCloses()
	if h.sessions == nil {
		return
	}
	h.sessions.Close(ctx)
}

func (h *Handler) OnReady(event *events.Ready) {
	slog.Info("gateway ready", slog.String("session_id", event.SessionID))
	slog.Info("slash command handlers are ready")
}

func (h *Handler) OnApplicationCommandInteractionCreate(event *events.ApplicationCommandInteractionCreate) {
	data := event.SlashCommandInteractionData()

	switch data.CommandName() {
	case ttsJoinCommandName:
		h.handleTTSJoin(event)
	case ttsDisconnectCommandName:
		h.handleTTSDisconnect(event)
	default:
		slog.Warn("received unsupported application command", slog.String("command_name", data.CommandName()))
	}
}

func (h *Handler) OnVoiceServerUpdate(event *events.VoiceServerUpdate) {
	if h.sessions == nil || !h.sessions.Exists(event.GuildID) {
		return
	}

	endpoint := ""
	if event.Endpoint != nil {
		endpoint = *event.Endpoint
	}

	attrs := []any{
		slog.Uint64("guild_id", uint64(event.GuildID)),
		slog.String("endpoint", endpoint),
	}
	if h.extendPendingVoiceGatewayClose(event.GuildID) {
		slog.Info("voice server update received while recovering voice session", attrs...)
		return
	}

	slog.Info("voice server update received for active session", attrs...)
}

func (h *Handler) OnGuildVoiceStateUpdate(event *events.GuildVoiceStateUpdate) {
	if h.sessions == nil {
		return
	}
	if event.VoiceState.UserID != event.Client().ID() {
		return
	}
	if event.VoiceState.ChannelID != nil {
		if h.hasPendingVoiceGatewayClose(event.VoiceState.GuildID) {
			slog.Info("bot voice state updated while recovering voice session",
				slog.Uint64("guild_id", uint64(event.VoiceState.GuildID)),
				slog.Uint64("voice_channel_id", uint64(*event.VoiceState.ChannelID)),
			)
		}
		return
	}
	guildID := event.VoiceState.GuildID
	h.cancelPendingVoiceGatewayClose(guildID)
	if !h.sessions.Exists(guildID) {
		return
	}

	slog.Info("bot voice state left channel, destroying session",
		slog.Uint64("guild_id", uint64(guildID)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), voiceCleanupTimeout)
	defer cancel()
	if err := h.sessions.Destroy(ctx, guildID); err != nil && !errors.Is(err, session.ErrSessionNotFound) {
		slog.Warn("failed to destroy session after bot voice disconnect",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(guildID)),
		)
	}
}

func (h *Handler) OnMessageCreate(event *events.MessageCreate) {
	if h.sessions == nil || event.GuildID == nil {
		return
	}

	sess, ok := h.sessions.Get(*event.GuildID)
	if !ok {
		return
	}
	if sess.TextChannelID() != event.ChannelID {
		return
	}
	if shouldSkipMessage(event, event.Client().ID()) {
		return
	}

	mentions := make([]tts.Mention, len(event.Message.Mentions))
	for i, user := range event.Message.Mentions {
		displayName := user.EffectiveName()
		if event.GuildID != nil {
			if client := event.Client(); client.Caches != nil {
				if member, ok := client.Caches.Member(*event.GuildID, user.ID); ok {
					displayName = member.EffectiveName()
				} else if member, err := client.Rest.GetMember(*event.GuildID, user.ID); err == nil {
					displayName = member.EffectiveName()
				}
			} else if member, err := event.Client().Rest.GetMember(*event.GuildID, user.ID); err == nil {
				displayName = member.EffectiveName()
			}
		}
		mentions[i] = tts.Mention{
			ID:          user.ID.String(),
			DisplayName: displayName,
		}
	}

	normalized := tts.NormalizeText(event.Message.Content, mentions)
	if normalized == "" {
		return
	}

	channelName := tts.DefaultChannelName
	if channel, ok := event.Channel(); ok {
		channelName = channel.Name()
	}

	textFilePath, err := tts.CreateTextFile(channelName, normalized, time.Now())
	if err != nil {
		slog.Error("failed to create text file for tts request",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(sess.GuildID())),
			slog.Uint64("channel_id", uint64(event.ChannelID)),
			slog.Uint64("message_id", uint64(event.MessageID)),
		)
		return
	}
	sess.TrackTempFile(textFilePath)

	request := session.PlaybackRequest{
		Content:      normalized,
		TextFilePath: textFilePath,
	}
	if err := sess.Enqueue(request); err != nil {
		_ = sess.RemoveTempFile(textFilePath)
		slog.Warn("failed to enqueue tts request",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(sess.GuildID())),
			slog.Uint64("channel_id", uint64(event.ChannelID)),
			slog.Uint64("message_id", uint64(event.MessageID)),
		)
		return
	}

	slog.Info("queued tts request",
		slog.Uint64("guild_id", uint64(sess.GuildID())),
		slog.Uint64("channel_id", uint64(event.ChannelID)),
		slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
		slog.Uint64("message_id", uint64(event.MessageID)),
		slog.Int("queue_length", sess.QueueLen()),
		slog.String("text_file_path", textFilePath),
	)
}

func (h *Handler) handleTTSJoin(event *events.ApplicationCommandInteractionCreate) {
	guildID := event.GuildID()
	if guildID == nil {
		h.respondEphemeral(event, "このコマンドはサーバー内でのみ使用できます。")
		return
	}
	if h.sessions != nil && h.sessions.Exists(*guildID) {
		slog.Info("rejected duplicate ttsjoin command",
			slog.Uint64("guild_id", uint64(*guildID)),
			slog.Uint64("actor_user_id", uint64(event.User().ID)),
		)
		h.respondEphemeral(event, "このサーバーでは既に読み上げセッションが起動しています。`/ttsdisconnect` を実行してから再度お試しください。")
		return
	}
	if err := event.CreateMessage(discord.NewMessageCreate().WithContent("ボイスチャンネルへ接続しています。しばらくお待ちください。").WithEphemeral(true)); err != nil {
		slog.Error("failed to create initial ttsjoin interaction response",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(*guildID)),
			slog.Uint64("actor_user_id", uint64(event.User().ID)),
		)
		return
	}

	go h.processTTSJoin(event, *guildID, event.Channel().ID(), event.User().ID)
}

func (h *Handler) processTTSJoin(event *events.ApplicationCommandInteractionCreate, guildID snowflake.ID, textChannelID snowflake.ID, actorUserID snowflake.ID) {
	finalized := false
	finishDeferred := func(content string) {
		finalized = true
		_ = h.updateDeferredResponse(event, content)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic while handling ttsjoin command",
				slog.Any("panic", recovered),
				slog.Uint64("guild_id", uint64(guildID)),
				slog.Uint64("actor_user_id", uint64(actorUserID)),
				slog.String("stack", string(debug.Stack())),
			)
			if !finalized {
				finishDeferred("読み上げセッションの開始中に内部エラーが発生しました。少し待ってから再度お試しください。")
			}
		}
	}()

	slog.Info("received ttsjoin command",
		slog.Uint64("guild_id", uint64(guildID)),
		slog.Uint64("channel_id", uint64(textChannelID)),
		slog.Uint64("actor_user_id", uint64(actorUserID)),
	)

	voiceState, err := h.resolveUserVoiceState(event.Client(), guildID, actorUserID)
	if err != nil {
		slog.Error("failed to resolve command user voice state",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Uint64("actor_user_id", uint64(actorUserID)),
		)
		finishDeferred("コマンド実行者の Voice State を取得できませんでした。少し待ってから再度お試しください。")
		return
	}
	if voiceState == nil || voiceState.ChannelID == nil {
		slog.Info("command user is not connected to a voice channel",
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Uint64("actor_user_id", uint64(actorUserID)),
		)
		finishDeferred("ボイスチャンネルに参加してから `/ttsjoin` を実行してください。")
		return
	}

	voiceChannelID := *voiceState.ChannelID
	sess, err := h.openVoiceSession(event.Client(), guildID, textChannelID, voiceChannelID)
	if err != nil {
		if errors.Is(err, session.ErrSessionAlreadyExists) {
			finishDeferred("このサーバーでは既に読み上げセッションが起動しています。`/ttsdisconnect` を実行してから再度お試しください。")
			return
		}

		slog.Error("failed to open tts voice session",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Uint64("voice_channel_id", uint64(voiceChannelID)),
		)
		finishDeferred("ボイスチャンネルへの接続に失敗しました。Bot に接続権限があるか確認して、再度お試しください。")
		return
	}

	// This bot only sends audio.
	// Reading inbound UDP packets here can surface transient DAVE decrypt errors
	// from other participants and incorrectly tear down the whole session.
	go h.runPlaybackWorker(sess)
	go h.runIdleVoiceKeepAlive(sess)

	if err := h.primeVoiceConnection(sess); err != nil {
		slog.Warn("failed to prime voice connection",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Uint64("voice_channel_id", uint64(voiceChannelID)),
		)
	}

	finishDeferred(
		fmt.Sprintf("ボイスチャンネル %s に接続しました。読み上げ対象テキストチャンネルは %s です。投稿の監視、キュー登録、音声再生を開始しました。", formatChannelMention(voiceChannelID), formatChannelMention(textChannelID)),
	)
}

func (h *Handler) runIdleVoiceKeepAlive(sess *session.Session) {
	ticker := time.NewTicker(audio.FrameDuration)
	defer ticker.Stop()

	for {
		select {
		case <-sess.Context().Done():
			return
		case <-ticker.C:
		}

		if h.hasPendingVoiceGatewayClose(sess.GuildID()) {
			continue
		}

		if err := sess.WithConnExclusive(func(conn voice.Conn) error {
			return h.sendIdleSilenceFrame(sess, conn)
		}); err != nil {
			if errors.Is(err, session.ErrSessionClosed) || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) || errors.Is(err, voice.ErrGatewayNotConnected) || errors.Is(err, discord.ErrShardNotReady) {
				continue
			}

			slog.Warn("failed to send idle voice keepalive",
				slog.Any("err", err),
				slog.Uint64("guild_id", uint64(sess.GuildID())),
				slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
			)
		}
	}
}

func (h *Handler) sendIdleSilenceFrame(sess *session.Session, conn voice.Conn) error {
	if conn == nil || conn.UDP() == nil {
		return session.ErrSessionClosed
	}

	if sess == nil {
		return session.ErrSessionClosed
	}

	if !sess.IdleSpeakingActive() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := conn.SetSpeaking(ctx, voice.SpeakingFlagMicrophone); err != nil {
			return fmt.Errorf("set speaking on for idle keepalive: %w", err)
		}
		sess.SetIdleSpeakingActive(true)
	}
	if _, err := conn.UDP().Write(voice.SilenceAudioFrame); err != nil {
		sess.SetIdleSpeakingActive(false)
		return fmt.Errorf("send idle silence frame: %w", err)
	}
	return nil
}

func (h *Handler) primeVoiceConnection(sess *session.Session) error {
	if sess == nil {
		return errors.New("voice session is not ready")
	}

	return sess.WithConnExclusive(func(conn voice.Conn) error {
		return h.sendIdleSilenceFrame(sess, conn)
	})
}

func (h *Handler) handleTTSDisconnect(event *events.ApplicationCommandInteractionCreate) {
	guildID := event.GuildID()
	if guildID == nil {
		h.respondEphemeral(event, "このコマンドはサーバー内でのみ使用できます。")
		return
	}
	if h.sessions == nil || !h.sessions.Exists(*guildID) {
		h.respondEphemeral(event, "現在、このサーバーで接続中の読み上げセッションはありません。")
		return
	}
	channelID := event.Channel().ID()
	actorUserID := event.User().ID

	if err := event.CreateMessage(discord.NewMessageCreate().WithContent("読み上げセッションを切断しています。しばらくお待ちください。").WithEphemeral(true)); err != nil {
		slog.Error("failed to create initial ttsdisconnect interaction response",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(*guildID)),
			slog.Uint64("actor_user_id", uint64(actorUserID)),
		)
		return
	}

	go h.processTTSDisconnect(*guildID, channelID, actorUserID, func(content string) {
		_ = h.updateDeferredResponse(event, content)
	})
}

func (h *Handler) processTTSDisconnect(guildID snowflake.ID, channelID snowflake.ID, actorUserID snowflake.ID, finishResponse func(string)) {
	finalized := false
	finish := func(content string) {
		finalized = true
		if finishResponse != nil {
			finishResponse(content)
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic while handling ttsdisconnect command",
				slog.Any("panic", recovered),
				slog.Uint64("guild_id", uint64(guildID)),
				slog.Uint64("actor_user_id", uint64(actorUserID)),
				slog.String("stack", string(debug.Stack())),
			)
			if !finalized {
				finish("読み上げセッションの切断中に内部エラーが発生しました。少し待ってから再度お試しください。")
			}
		}
	}()

	slog.Info("received ttsdisconnect command",
		slog.Uint64("guild_id", uint64(guildID)),
		slog.Uint64("channel_id", uint64(channelID)),
		slog.Uint64("actor_user_id", uint64(actorUserID)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), voiceConnectTimeout)
	defer cancel()

	if h.sessions == nil {
		finish("現在、このサーバーで接続中の読み上げセッションはありません。")
		return
	}

	if err := h.sessions.Destroy(ctx, guildID); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			slog.Info("ttsdisconnect completed after session was already closed",
				slog.Uint64("guild_id", uint64(guildID)),
				slog.Uint64("actor_user_id", uint64(actorUserID)),
			)
			finish("読み上げセッションは既に切断されています。")
			return
		}

		slog.Error("failed to destroy voice session",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(guildID)),
		)
		finish("読み上げセッションの切断に失敗しました。少し待ってから再度お試しください。")
		return
	}

	finish("読み上げセッションを切断しました。")
}

func (h *Handler) respondEphemeral(event *events.ApplicationCommandInteractionCreate, content string) {
	if err := event.CreateMessage(discord.NewMessageCreate().WithContent(content).WithEphemeral(true)); err != nil {
		slog.Error("failed to create interaction response",
			slog.Any("err", err),
			slog.Uint64("interaction_id", uint64(event.ID())),
		)
	}
}

func (h *Handler) updateDeferredResponse(event *events.ApplicationCommandInteractionCreate, content string) error {
	if _, err := event.Client().Rest.UpdateInteractionResponse(
		event.ApplicationID(),
		event.Token(),
		discord.NewMessageUpdate().WithContent(content),
	); err != nil {
		slog.Error("failed to update deferred interaction response",
			slog.Any("err", err),
			slog.Uint64("interaction_id", uint64(event.ID())),
		)
		return err
	}
	return nil
}

func (h *Handler) resolveUserVoiceState(client *disgobot.Client, guildID snowflake.ID, userID snowflake.ID) (*discord.VoiceState, error) {
	if client.Caches != nil {
		if voiceState, ok := client.Caches.VoiceState(guildID, userID); ok {
			return &voiceState, nil
		}
	}

	voiceState, err := client.Rest.GetUserVoiceState(guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("get user voice state from rest: %w", err)
	}
	return voiceState, nil
}

func (h *Handler) openVoiceSession(client *disgobot.Client, guildID snowflake.ID, textChannelID snowflake.ID, voiceChannelID snowflake.ID) (*session.Session, error) {
	if h.sessions == nil {
		return nil, errors.New("session manager is not initialized")
	}

	conn := client.VoiceManager.CreateConn(guildID)
	h.attachVoiceConnEventHandler(guildID, conn)
	sess, err := h.sessions.Create(session.CreateParams{
		GuildID:        guildID,
		TextChannelID:  textChannelID,
		VoiceChannelID: voiceChannelID,
		Conn:           conn,
		BeforeClose: func() {
			client.VoiceManager.RemoveConn(guildID)
		},
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), voiceConnectTimeout)
	defer cancel()
	if err := conn.Open(ctx, voiceChannelID, false, false); err != nil {
		h.cleanupFailedVoiceSession(client, guildID)
		return nil, fmt.Errorf("open voice connection: %w", err)
	}

	slog.Info("voice session started",
		slog.Uint64("guild_id", uint64(guildID)),
		slog.Uint64("text_channel_id", uint64(textChannelID)),
		slog.Uint64("voice_channel_id", uint64(voiceChannelID)),
	)

	return sess, nil
}

func (h *Handler) cleanupFailedVoiceSession(client *disgobot.Client, guildID snowflake.ID) {
	if client != nil && client.VoiceManager != nil {
		client.VoiceManager.RemoveConn(guildID)
	}

	if h.sessions == nil {
		return
	}

	sess, err := h.sessions.Detach(guildID)
	if err != nil {
		if !errors.Is(err, session.ErrSessionNotFound) {
			slog.Warn("failed to detach voice session after open failure",
				slog.Any("err", err),
				slog.Uint64("guild_id", uint64(guildID)),
			)
		}
		return
	}

	go func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), voiceCleanupTimeout)
		defer cancel()
		sess.Close(closeCtx)
	}()
}

func (h *Handler) runPlaybackWorker(sess *session.Session) {
	if h.synthesizer == nil {
		slog.Warn("tts synthesizer is not configured", slog.Uint64("guild_id", uint64(sess.GuildID())))
		return
	}

	for {
		select {
		case <-sess.Context().Done():
			return
		case request := <-sess.Queue():
			if sess.Context().Err() != nil {
				return
			}
			h.processPlaybackRequest(sess, request)
		}
	}
}

func (h *Handler) processPlaybackRequest(sess *session.Session, request session.PlaybackRequest) {
	result, err := h.synthesizer.Synthesize(request.TextFilePath, time.Now())
	if removeErr := sess.RemoveTempFile(request.TextFilePath); removeErr != nil {
		slog.Warn("failed to remove synthesized text file",
			slog.Any("err", removeErr),
			slog.Uint64("guild_id", uint64(sess.GuildID())),
			slog.String("text_file_path", request.TextFilePath),
		)
	}

	if err != nil {
		logSynthesisError(sess, request, err)
		return
	}
	if result.AudioSource == nil {
		slog.Error("tts synthesizer returned no audio source",
			slog.Uint64("guild_id", uint64(sess.GuildID())),
			slog.Uint64("text_channel_id", uint64(sess.TextChannelID())),
			slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
			slog.String("text_file_path", request.TextFilePath),
		)
		return
	}

	audioSource := result.AudioSource

	defer func() {
		if cleanupErr := audioSource.Cleanup(); cleanupErr != nil {
			slog.Warn("failed to clean up generated audio source",
				slog.Any("err", cleanupErr),
				slog.Uint64("guild_id", uint64(sess.GuildID())),
				slog.String("audio_source", audioSource.Description()),
			)
		}
	}()

	slog.Info("generated tts audio from queued message",
		slog.Uint64("guild_id", uint64(sess.GuildID())),
		slog.Uint64("text_channel_id", uint64(sess.TextChannelID())),
		slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
		slog.String("audio_source", audioSource.Description()),
		slog.String("content", request.Content),
	)

	stream := mustNewWAVStreamFromSource(audioSource)
	if err := sendPlaybackStream(sess, stream); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		slog.Error("failed to play generated tts audio",
			slog.Any("err", err),
			slog.Uint64("guild_id", uint64(sess.GuildID())),
			slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
			slog.String("audio_source", audioSource.Description()),
		)
		return
	}

	slog.Info("completed tts audio playback",
		slog.Uint64("guild_id", uint64(sess.GuildID())),
		slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
		slog.String("audio_source", audioSource.Description()),
	)
}

func sendPlaybackStream(sess *session.Session, stream audio.OpusFrameStream) (err error) {
	used := false
	defer func() {
		if !used && stream != nil {
			_ = stream.Close()
		}
	}()

	return sess.WithConnExclusive(func(conn voice.Conn) error {
		sess.SetIdleSpeakingActive(false)
		defer sess.SetIdleSpeakingActive(false)
		used = true
		return audio.SendOpusFrameStream(sess.Context(), conn, stream)
	})
}

func (h *Handler) WaitForVoiceServerRecovery(guildID snowflake.ID, fallback func()) {
	if fallback == nil {
		return
	}
	if h == nil || h.sessions == nil || !h.sessions.Exists(guildID) {
		fallback()
		return
	}
	if sess, ok := h.sessions.Get(guildID); ok {
		sess.SetIdleSpeakingActive(false)
	}

	gracePeriod := h.voiceServerUpdateGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = defaultVoiceServerUpdateGracePeriod
	}

	h.voiceGatewayCloseMu.Lock()
	if h.voiceGatewayRecoveries == nil {
		h.voiceGatewayRecoveries = make(map[snowflake.ID]*voiceGatewayRecovery)
	}
	if existing, ok := h.voiceGatewayRecoveries[guildID]; ok && existing.timer != nil {
		existing.timer.Stop()
	}
	recovery := &voiceGatewayRecovery{fallback: fallback}
	recovery.timer = time.AfterFunc(gracePeriod, func() {
		h.voiceGatewayCloseMu.Lock()
		current, ok := h.voiceGatewayRecoveries[guildID]
		shouldFallback := ok && current == recovery
		if shouldFallback {
			delete(h.voiceGatewayRecoveries, guildID)
		}
		h.voiceGatewayCloseMu.Unlock()
		if !shouldFallback {
			return
		}

		if h.sessions == nil || !h.sessions.Exists(guildID) {
			return
		}

		slog.Warn("voice server update was not received in time, closing stalled voice session",
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Duration("grace_period", gracePeriod),
		)
		recovery.fallback()
	})
	h.voiceGatewayRecoveries[guildID] = recovery
	h.voiceGatewayCloseMu.Unlock()

	slog.Warn("voice gateway reported call terminated, waiting for a follow-up voice server update",
		slog.Uint64("guild_id", uint64(guildID)),
		slog.Duration("grace_period", gracePeriod),
	)
}

func (h *Handler) extendPendingVoiceGatewayClose(guildID snowflake.ID) bool {
	if h == nil {
		return false
	}

	gracePeriod := h.voiceServerUpdateGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = defaultVoiceServerUpdateGracePeriod
	}

	h.voiceGatewayCloseMu.Lock()
	recovery, ok := h.voiceGatewayRecoveries[guildID]
	if !ok {
		h.voiceGatewayCloseMu.Unlock()
		return false
	}
	if recovery.timer != nil {
		recovery.timer.Stop()
	}
	recovery.timer = time.AfterFunc(gracePeriod, func() {
		h.voiceGatewayCloseMu.Lock()
		current, exists := h.voiceGatewayRecoveries[guildID]
		shouldFallback := exists && current == recovery
		if shouldFallback {
			delete(h.voiceGatewayRecoveries, guildID)
		}
		h.voiceGatewayCloseMu.Unlock()
		if !shouldFallback {
			return
		}

		if h.sessions == nil || !h.sessions.Exists(guildID) {
			return
		}

		slog.Warn("voice gateway recovery did not complete in time after voice server update",
			slog.Uint64("guild_id", uint64(guildID)),
			slog.Duration("grace_period", gracePeriod),
		)
		recovery.fallback()
	})
	h.voiceGatewayCloseMu.Unlock()
	return true
}

func (h *Handler) attachVoiceConnEventHandler(guildID snowflake.ID, conn voice.Conn) {
	if conn == nil {
		return
	}

	conn.SetEventHandlerFunc(func(gateway voice.Gateway, opCode voice.Opcode, sequenceNumber int, data voice.GatewayMessageData) {
		h.onVoiceGatewayEvent(guildID, opCode)
	})
}

func (h *Handler) onVoiceGatewayEvent(guildID snowflake.ID, opCode voice.Opcode) {
	switch opCode {
	case voice.OpcodeResumed, voice.OpcodeSessionDescription:
		if h.cancelPendingVoiceGatewayClose(guildID) {
			slog.Info("voice gateway recovery completed",
				slog.Uint64("guild_id", uint64(guildID)),
				slog.Int("opcode", int(opCode)),
			)
		}
	}
}

func (h *Handler) hasPendingVoiceGatewayClose(guildID snowflake.ID) bool {
	if h == nil {
		return false
	}

	h.voiceGatewayCloseMu.Lock()
	_, ok := h.voiceGatewayRecoveries[guildID]
	h.voiceGatewayCloseMu.Unlock()
	return ok
}

func (h *Handler) cancelPendingVoiceGatewayClose(guildID snowflake.ID) bool {
	if h == nil {
		return false
	}

	h.voiceGatewayCloseMu.Lock()
	recovery, ok := h.voiceGatewayRecoveries[guildID]
	if ok {
		delete(h.voiceGatewayRecoveries, guildID)
	}
	h.voiceGatewayCloseMu.Unlock()

	if ok && recovery.timer != nil {
		recovery.timer.Stop()
	}
	return ok
}

func (h *Handler) cancelAllPendingVoiceGatewayCloses() {
	if h == nil {
		return
	}

	h.voiceGatewayCloseMu.Lock()
	timers := make([]*time.Timer, 0, len(h.voiceGatewayRecoveries))
	for guildID, recovery := range h.voiceGatewayRecoveries {
		delete(h.voiceGatewayRecoveries, guildID)
		if recovery != nil && recovery.timer != nil {
			timers = append(timers, recovery.timer)
		}
	}
	h.voiceGatewayCloseMu.Unlock()

	for _, timer := range timers {
		timer.Stop()
	}
}

func formatChannelMention(channelID snowflake.ID) string {
	return fmt.Sprintf("<#%d>", channelID)
}

func shouldSkipMessage(event *events.MessageCreate, botUserID snowflake.ID) bool {
	if event.Message.Type.System() {
		return true
	}
	if event.Message.WebhookID != nil {
		return true
	}
	if event.Message.Author.Bot || event.Message.Author.ID == botUserID {
		return true
	}
	if event.Message.Content == "" {
		return true
	}
	return false
}

func logSynthesisError(sess *session.Session, request session.PlaybackRequest, err error) {
	attrs := []any{
		slog.Any("err", err),
		slog.Uint64("guild_id", uint64(sess.GuildID())),
		slog.Uint64("text_channel_id", uint64(sess.TextChannelID())),
		slog.Uint64("voice_channel_id", uint64(sess.VoiceChannelID())),
		slog.String("text_file_path", request.TextFilePath),
	}

	var synthesisErr *tts.SynthesisError
	if errors.As(err, &synthesisErr) {
		attrs = append(attrs,
			slog.String("stderr", synthesisErr.Stderr),
			slog.String("audio_file_path", synthesisErr.OutputFilePath),
		)
	}

	slog.Error("failed to synthesize wav from queued message", attrs...)
}

func mustNewWAVStreamFromSource(source tts.AudioSource) audio.OpusFrameStream {
	if source == nil {
		return &errorStream{err: errors.New("audio source is nil")}
	}

	reader, err := source.Open()
	if err != nil {
		return &errorStream{err: err}
	}
	defer reader.Close()

	stream, err := audio.NewWAVStreamFromReader(reader)
	if err != nil {
		return &errorStream{err: err}
	}
	return stream
}

type errorStream struct {
	err error
}

func (s *errorStream) NextOpusFrame() ([]byte, error) {
	return nil, s.err
}

func (s *errorStream) Close() error {
	return nil

}
