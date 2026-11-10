package appbot

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"
)

const voiceGatewayCloseCodeCallTerminated = 4022

func NewVoiceConnCreateFunc(handler *Handler) voice.ConnCreateFunc {
	return func(guildID snowflake.ID, userID snowflake.ID, voiceStateUpdateFunc voice.StateUpdateFunc, removeConnFunc func(), opts ...voice.ConnConfigOpt) voice.Conn {
		connOpts := append([]voice.ConnConfigOpt{}, opts...)
		if handler != nil {
			connOpts = append(connOpts, voice.WithConnEventHandlerFunc(
				func(_ voice.Gateway, opCode voice.Opcode, _ int, _ voice.GatewayMessageData) {
					handler.onVoiceGatewayEvent(guildID, opCode)
				},
			))
		}
		connOpts = append(connOpts, voice.WithConnGatewayCreateFunc(
			func(
				daveSession godave.Session,
				eventHandlerFunc voice.EventHandlerFunc,
				closeHandlerFunc voice.CloseHandlerFunc,
				gatewayOpts ...voice.GatewayConfigOpt,
			) voice.Gateway {
				wrappedCloseHandler := closeHandlerFunc
				if handler != nil {
					wrappedCloseHandler = func(gateway voice.Gateway, err error) {
						if isVoiceGatewayCloseCode(err, voiceGatewayCloseCodeCallTerminated) {
							handler.WaitForVoiceServerRecovery(guildID, func() {
								closeHandlerFunc(gateway, err)
							})
							return
						}
						closeHandlerFunc(gateway, err)
					}
				}

				return newRecoverableGateway(daveSession, eventHandlerFunc, wrappedCloseHandler, gatewayOpts...)
			},
		))
		return voice.NewConn(guildID, userID, voiceStateUpdateFunc, removeConnFunc, connOpts...)
	}
}

type recoverableGateway struct {
	mu                 sync.RWMutex
	current            voice.Gateway
	closeHandler       voice.CloseHandlerFunc
	newUnderlying      func(closeHandler voice.CloseHandlerFunc) voice.Gateway
	recreateOnNextOpen bool
}

func newRecoverableGateway(
	daveSession godave.Session,
	eventHandlerFunc voice.EventHandlerFunc,
	closeHandlerFunc voice.CloseHandlerFunc,
	opts ...voice.GatewayConfigOpt,
) voice.Gateway {
	gateway := &recoverableGateway{
		closeHandler: closeHandlerFunc,
	}
	gateway.newUnderlying = func(closeHandler voice.CloseHandlerFunc) voice.Gateway {
		return voice.NewGateway(daveSession, eventHandlerFunc, closeHandler, opts...)
	}
	gateway.current = gateway.createUnderlying()
	return gateway
}

func (g *recoverableGateway) createUnderlying() voice.Gateway {
	return g.newUnderlying(func(_ voice.Gateway, err error) {
		if isVoiceGatewayCloseCode(err, voiceGatewayCloseCodeCallTerminated) {
			g.mu.Lock()
			g.recreateOnNextOpen = true
			g.mu.Unlock()
		}
		if g.closeHandler != nil {
			g.closeHandler(g, err)
		}
	})
}

func (g *recoverableGateway) currentGateway() voice.Gateway {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.current
}

func (g *recoverableGateway) SSRC() uint32 {
	if current := g.currentGateway(); current != nil {
		return current.SSRC()
	}
	return 0
}

func (g *recoverableGateway) Open(ctx context.Context, state voice.State) error {
	g.mu.Lock()
	if g.current == nil || g.recreateOnNextOpen {
		if g.current != nil {
			g.current.Close()
		}
		g.current = g.createUnderlying()
		g.recreateOnNextOpen = false
	}
	current := g.current
	g.mu.Unlock()

	if current == nil {
		return errors.New("voice gateway is not initialized")
	}
	return current.Open(ctx, state)
}

func (g *recoverableGateway) Close() {
	if current := g.currentGateway(); current != nil {
		current.Close()
	}
}

func (g *recoverableGateway) CloseWithCode(code int, message string) {
	if current := g.currentGateway(); current != nil {
		current.CloseWithCode(code, message)
	}
}

func (g *recoverableGateway) Status() voice.Status {
	if current := g.currentGateway(); current != nil {
		return current.Status()
	}
	return voice.StatusDisconnected
}

func (g *recoverableGateway) Send(ctx context.Context, opCode voice.Opcode, data voice.GatewayMessageData) error {
	current := g.currentGateway()
	if current == nil {
		return errors.New("voice gateway is not initialized")
	}
	return current.Send(ctx, opCode, data)
}

func (g *recoverableGateway) Latency() time.Duration {
	if current := g.currentGateway(); current != nil {
		return current.Latency()
	}
	return 0
}

func isVoiceGatewayCloseCode(err error, code int) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == code
}
