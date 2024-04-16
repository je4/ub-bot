package discord

import (
	"emperror.dev/errors"
	"github.com/bwmarrin/discordgo"
)

func (d *Session) NewInteraction(interaction *discordgo.Interaction) *Interaction {
	return &Interaction{
		session:     d.session,
		Interaction: interaction,
	}
}

type Interaction struct {
	*discordgo.Interaction
	session *discordgo.Session
}

func (i *Interaction) GetSession() *discordgo.Session {
	return i.session
}

func (i *Interaction) SendInteractionResponseMessage(msg string) error {
	if err := i.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
		},
	}); err != nil {
		return errors.Wrap(err, "cannot send interaction response")
	}
	return nil
}

func (i *Interaction) SendInteractionResponseEmbeds(embeds []*discordgo.MessageEmbed) error {
	if err := i.session.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: embeds,
		},
	}); err != nil {
		return errors.Wrap(err, "cannot send interaction response")
	}
	return nil
}

func (i *Interaction) SendChannelEmbeds(embeds []*discordgo.MessageEmbed) error {
	for _, embed := range embeds {
		if _, err := i.session.ChannelMessageSendEmbed(i.ChannelID, embed); err != nil {
			return errors.Wrap(err, "cannot send channel embeds")
		}
	}
	return nil
}

func (i *Interaction) SendChannelMessage(msg string) error {
	if _, err := i.session.ChannelMessageSend(i.ChannelID, msg); err != nil {
		return errors.Wrap(err, "cannot send channel message")
	}
	return nil
}
