package bot

import (
	"context"
	"log"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/agent-proxy/internal/claude"
	"github.com/agent-proxy/internal/config"
	"github.com/agent-proxy/internal/middleware"
	"github.com/agent-proxy/internal/session"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	handler  *Handler
	executor *claude.Executor
}

func New(cfg *config.Config) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, err
	}

	log.Printf("authorized on telegram as @%s", api.Self.UserName)

	sender := NewSender(api, cfg.MaxMessageLen, cfg.UpdateInterval)
	executor := claude.NewExecutor(cfg.ClaudePath, cfg.Timeout)
	sessions := session.NewManager(cfg.DefaultWorkDir)
	auth := middleware.NewAuth(cfg.AllowedUsers)
	handler := NewHandler(sender, executor, sessions, auth)

	return &Bot{
		api:      api,
		handler:  handler,
		executor: executor,
	}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	log.Println("bot started, waiting for messages...")

	for {
		select {
		case <-ctx.Done():
			log.Println("stopping bot...")
			b.executor.KillAll()
			b.api.StopReceivingUpdates()
			return nil
		case update := <-updates:
			if update.CallbackQuery != nil {
				go b.handler.HandleCallback(ctx, *update.CallbackQuery)
			} else {
				go b.handler.Handle(ctx, update)
			}
		}
	}
}
