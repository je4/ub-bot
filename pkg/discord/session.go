package discord

import (
	"emperror.dev/errors"
	"github.com/bwmarrin/discordgo"
	"github.com/je4/utils/v2/pkg/zLogger"
)

type InterActionsCreateFunc func(s *discordgo.Session, i *discordgo.InteractionCreate)
type CommandCreate func(i *Interaction)

func NewSession(token string, appID, guildID string, logger zLogger.ZLogger) (*Session, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create discord session")
	}
	s := &Session{
		session:      session,
		appID:        appID,
		guildID:      guildID,
		logger:       logger,
		cmdIDs:       make(map[string]string),
		interactions: map[string]InterActionsCreateFunc{},
	}
	return s, s.init()
}

type Session struct {
	session      *discordgo.Session
	interactions map[string]InterActionsCreateFunc
	logger       zLogger.ZLogger
	appID        string
	guildID      string
	cmdIDs       map[string]string
}

func (d *Session) init() error {
	d.session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		d.ready(r)
	})
	d.session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := d.interactions[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})
	return nil
}

func (d *Session) Open() error {
	return errors.Wrap(d.session.Open(), "cannot open discord session")
}

func (d *Session) Close() error {
	var errs []error
	for id, name := range d.cmdIDs {
		d.logger.Info().Msgf("Deleting command %s", name)
		if err := d.session.ApplicationCommandDelete(d.appID, d.guildID, id); err != nil {
			errs = append(errs, errors.Wrapf(err, "cannot delete command %s", name))
			d.logger.Error().Err(err).Msgf("Cannot delete command %s", name)
		}
	}
	if err := d.session.Close(); err != nil {
		errs = append(errs, errors.Wrap(err, "cannot close discord session"))
	}
	return errors.Combine(errs...)
}

func (d *Session) ApplicationCommandCreate(createFunc CommandCreate, cmd *discordgo.ApplicationCommand, options ...discordgo.RequestOption) error {
	d.interactions[cmd.Name] = func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		createFunc(d.NewInteraction(i.Interaction))
	}
	rCmd, err := d.session.ApplicationCommandCreate(d.appID, d.guildID, cmd, options...)
	if err != nil {
		return errors.Wrap(err, "cannot create application command")
	}
	d.cmdIDs[rCmd.ID] = rCmd.Name
	d.logger.Info().Msgf("Command %s created", rCmd.Name)
	return nil
}

func (d *Session) ready(r *discordgo.Ready) {
	d.logger.Info().Msg("Bot is up!")
	for _, guild := range r.Guilds {
		d.logger.Info().Msgf("Guild: %s\n", guild.ID)
	}
}
