package main

import (
	"context"
	"log/slog"

	"github.com/puzpuzpuz/xsync/v3"
)

type EventType string

type EventHub struct {
	events   chan *event
	handlers *xsync.MapOf[EventType, EventHandler]
	logger   *slog.Logger
}

type EventHandler func(payload any) (err error)

type event struct {
	Type    EventType
	Payload any
}

func NewEventHub(logger *slog.Logger) (eh *EventHub) {
	return &EventHub{
		events:   make(chan *event, 8),
		handlers: xsync.NewMapOf[EventType, EventHandler](),
		logger:   logger,
	}
}

func (eh *EventHub) RegisterHandler(eventType EventType, handler EventHandler, concurrent bool) {
	eh.handlers.Store(eventType, handler)
}

func (eh *EventHub) Run(ctx context.Context) {
out:
	for {
		select {
		case event := <-eh.events:
			handler, ok := eh.handlers.Load(event.Type)
			if !ok {
				eh.logger.Warn("unknown event type, drop it", "type", event.Type)
				continue
			}

			err := handler(event.Payload)
			if err != nil {
				eh.logger.Error("handle event error", "type", event.Type, "error", err)
				continue
			}
		case <-ctx.Done():
			eh.logger.Debug("context exceeded, exit")
			break out
		}
	}

	return
}

func (eh *EventHub) Emit(eventType EventType, payload any) {
	eh.events <- &event{
		Type:    eventType,
		Payload: payload,
	}
	return
}
