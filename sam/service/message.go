package service

import (
	"context"
	"regexp"
	"strings"

	"github.com/pkg/errors"

	"github.com/crusttech/crust/internal/payload"
	"github.com/crusttech/crust/sam/repository"
	"github.com/crusttech/crust/sam/types"
	systemService "github.com/crusttech/crust/system/service"
	systemTypes "github.com/crusttech/crust/system/types"
)

type (
	message struct {
		db  db
		ctx context.Context

		attachment repository.AttachmentRepository
		channel    repository.ChannelRepository
		cmember    repository.ChannelMemberRepository
		unreads    repository.UnreadRepository
		message    repository.MessageRepository
		mflag      repository.MessageFlagRepository
		mentions   repository.MentionRepository

		usr systemService.UserService
		evl EventService
	}

	MessageService interface {
		With(ctx context.Context) MessageService

		Find(filter *types.MessageFilter) (types.MessageSet, error)
		FindThreads(filter *types.MessageFilter) (types.MessageSet, error)

		Create(messages *types.Message) (*types.Message, error)
		Update(messages *types.Message) (*types.Message, error)

		React(messageID uint64, reaction string) error
		RemoveReaction(messageID uint64, reaction string) error

		MarkAsRead(channelID, threadID, lastReadMessageID uint64) (uint32, error)

		Pin(messageID uint64) error
		RemovePin(messageID uint64) error

		Bookmark(messageID uint64) error
		RemoveBookmark(messageID uint64) error

		Delete(ID uint64) error
	}
)

const (
	settingsMessageBodyLength = 0
	mentionRE                 = `<([@#])(\d+)((?:\s)([^>]+))?>`
)

var (
	mentionsFinder = regexp.MustCompile(mentionRE)
)

func Message() MessageService {
	return &message{
		usr: systemService.DefaultUser,
		evl: DefaultEvent,
	}
}

func (svc *message) With(ctx context.Context) MessageService {
	db := repository.DB(ctx)
	return &message{
		db:  db,
		ctx: ctx,

		usr: svc.usr.With(ctx),
		evl: svc.evl.With(ctx),

		attachment: repository.Attachment(ctx, db),
		channel:    repository.Channel(ctx, db),
		cmember:    repository.ChannelMember(ctx, db),
		unreads:    repository.ChannelView(ctx, db),
		message:    repository.Message(ctx, db),
		mflag:      repository.MessageFlag(ctx, db),
		mentions:   repository.Mention(ctx, db),
	}
}

func (svc *message) Find(filter *types.MessageFilter) (mm types.MessageSet, err error) {
	// @todo get user from context
	filter.CurrentUserID = repository.Identity(svc.ctx)

	// @todo verify if current user can access & read from this channel
	_ = filter.ChannelID

	mm, err = svc.message.FindMessages(filter)
	if err != nil {
		return nil, err
	}

	return mm, svc.preload(mm)
}

func (svc *message) FindThreads(filter *types.MessageFilter) (mm types.MessageSet, err error) {
	// @todo get user from context
	filter.CurrentUserID = repository.Identity(svc.ctx)

	// @todo verify if current user can access & read from this channel
	_ = filter.ChannelID

	mm, err = svc.message.FindThreads(filter)
	if err != nil {
		return nil, err
	}

	return mm, svc.preload(mm)
}

func (svc *message) Create(in *types.Message) (message *types.Message, err error) {
	if in == nil {
		in = &types.Message{}
	}

	in.Message = strings.TrimSpace(in.Message)
	var mlen = len(in.Message)

	if mlen == 0 {
		return nil, errors.Errorf("Refusing to create message without contents")
	} else if settingsMessageBodyLength > 0 && mlen > settingsMessageBodyLength {
		return nil, errors.Errorf("Message length (%d characters) too long (max: %d)", mlen, settingsMessageBodyLength)
	}

	// @todo get user from context
	var currentUserID uint64 = repository.Identity(svc.ctx)

	in.UserID = currentUserID

	return message, svc.db.Transaction(func() (err error) {
		// Broadcast queue
		var bq = types.MessageSet{}

		if in.ReplyTo > 0 {
			var original *types.Message
			var replyTo = in.ReplyTo

			for replyTo > 0 {
				// Find original message
				original, err = svc.message.FindMessageByID(in.ReplyTo)
				if err != nil {
					return
				}

				replyTo = original.ReplyTo
			}

			if !original.Type.IsRepliable() {
				return errors.Errorf("Unable to reply on this message (type = %s)", original.Type)
			}

			// We do not want to have multi-level threads
			// Take original's reply-to and use it
			in.ReplyTo = original.ID

			in.ChannelID = original.ChannelID

			// Increment counter, on struct and in repostiry.
			original.Replies++
			if err = svc.message.IncReplyCount(original.ID); err != nil {
				return
			}

			// Broadcast updated original
			bq = append(bq, original)
		}

		if in.ChannelID == 0 {
			return errors.New("ChannelID missing")
		}

		// @todo [SECURITY] verify if current user can access & write to this channel

		if message, err = svc.message.CreateMessage(in); err != nil {
			return
		}

		if err = svc.updateMentions(message.ID, svc.extractMentions(message)); err != nil {
			return
		}

		if err = svc.unreads.Inc(message.ChannelID, message.ReplyTo, message.UserID); err != nil {
			return
		}

		return svc.sendEvent(append(bq, message)...)
	})
}

func (svc *message) Update(in *types.Message) (message *types.Message, err error) {
	if in == nil {
		in = &types.Message{}
	}

	in.Message = strings.TrimSpace(in.Message)
	var mlen = len(in.Message)

	if mlen == 0 {
		return nil, errors.Errorf("Refusing to update message without contents")
	} else if settingsMessageBodyLength > 0 && mlen > settingsMessageBodyLength {
		return nil, errors.Errorf("Message length (%d characters) too long (max: %d)", mlen, settingsMessageBodyLength)
	}

	// @todo get user from context
	var currentUserID uint64 = repository.Identity(svc.ctx)

	// @todo verify if current user can access & write to this channel
	_ = currentUserID

	return message, svc.db.Transaction(func() (err error) {
		message, err = svc.message.FindMessageByID(in.ID)
		if err != nil {
			return errors.Wrap(err, "Could not load message for editing")
		}

		if message.Message == in.Message {
			// Nothing changed
			return nil
		}

		if message.UserID != currentUserID {
			return errors.New("Not an owner")
		}

		// Allow message content to be changed
		message.Message = in.Message

		if message, err = svc.message.UpdateMessage(message); err != nil {
			return err
		}

		if err = svc.updateMentions(message.ID, svc.extractMentions(message)); err != nil {
			return
		}

		return svc.sendEvent(message)
	})
}

func (svc *message) Delete(ID uint64) error {
	// @todo get user from context
	var currentUserID uint64 = repository.Identity(svc.ctx)

	// @todo verify if current user can access & write to this channel
	_ = currentUserID

	// @todo load current message
	// @todo verify ownership

	return svc.db.Transaction(func() (err error) {
		// Broadcast queue
		var bq = types.MessageSet{}
		var deletedMsg, original *types.Message

		deletedMsg, err = svc.message.FindMessageByID(ID)
		if err != nil {
			return err
		}

		if deletedMsg.ReplyTo > 0 {
			original, err = svc.message.FindMessageByID(deletedMsg.ReplyTo)
			if err != nil {
				return err
			}

			// This is a reply to another message, decrease reply counter on the original, on struct and in the
			// repository
			if original.Replies > 0 {
				original.Replies--
			}

			if err = svc.message.DecReplyCount(original.ID); err != nil {
				return err
			}

			// Broadcast updated original
			bq = append(bq, original)
		}

		if err = svc.message.DeleteMessageByID(ID); err != nil {
			return
		}

		if err = svc.unreads.Dec(deletedMsg.ChannelID, deletedMsg.ReplyTo, deletedMsg.UserID); err != nil {
			return err
		} else {
			// Set deletedAt timestamp so that our clients can react properly...
			deletedMsg.DeletedAt = timeNowPtr()
		}

		if err = svc.updateMentions(ID, nil); err != nil {
			return
		}

		return svc.sendEvent(append(bq, deletedMsg)...)
	})
}

// M
func (svc *message) MarkAsRead(channelID, threadID, lastReadMessageID uint64) (count uint32, err error) {
	var currentUserID uint64 = repository.Identity(svc.ctx)

	err = svc.db.Transaction(func() (err error) {
		var ch *types.Channel
		var thread *types.Message
		var lastMessage *types.Message

		// Validate channel
		if ch, err = svc.channel.FindChannelByID(channelID); err != nil {
			return errors.Wrap(err, "unable to verify channel")
		} else if !ch.IsValid() {
			return errors.New("invalid channel")
		}

		if threadID > 0 {
			// Validate thread
			if thread, err = svc.message.FindMessageByID(threadID); err != nil {
				return errors.Wrap(err, "unable to verify thread")
			} else if !thread.IsValid() {
				return errors.New("invalid thread")
			}
		}

		if lastReadMessageID > 0 {
			// Validate thread
			if lastMessage, err = svc.message.FindMessageByID(lastReadMessageID); err != nil {
				return errors.Wrap(err, "unable to verify last message")
			} else if !lastMessage.IsValid() {
				return errors.New("invalid message")
			}
		}

		count, err = svc.message.CountFromMessageID(channelID, threadID, lastReadMessageID)
		if err != nil {
			return errors.Wrap(err, "unable to count unread messages")
		}

		err = svc.unreads.Record(currentUserID, channelID, threadID, lastReadMessageID, count)
		return errors.Wrap(err, "unable to record unread messages")
	})

	return count, errors.Wrap(err, "unable to mark as read")
}

// React on a message with an emoji
func (svc *message) React(messageID uint64, reaction string) error {
	return svc.flag(messageID, reaction, false)
}

// Remove reaction on a message
func (svc *message) RemoveReaction(messageID uint64, reaction string) error {
	return svc.flag(messageID, reaction, true)
}

// Pin message to the channel
func (svc *message) Pin(messageID uint64) error {
	return svc.flag(messageID, types.MessageFlagPinnedToChannel, false)
}

// Remove pin from message
func (svc *message) RemovePin(messageID uint64) error {
	return svc.flag(messageID, types.MessageFlagPinnedToChannel, true)
}

// Bookmark message (private)
func (svc *message) Bookmark(messageID uint64) error {
	return svc.flag(messageID, types.MessageFlagBookmarkedMessage, false)
}

// Remove bookmark message (private)
func (svc *message) RemoveBookmark(messageID uint64) error {
	return svc.flag(messageID, types.MessageFlagBookmarkedMessage, true)
}

// React on a message with an emoji
func (svc *message) flag(messageID uint64, flag string, remove bool) error {
	// @todo get user from context
	var currentUserID uint64 = repository.Identity(svc.ctx)

	// @todo verify if current user can access & write to this channel
	_ = currentUserID

	if strings.TrimSpace(flag) == "" {
		// Sanitize
		flag = types.MessageFlagPinnedToChannel
	}

	// @todo validate flags beyond empty string

	err := svc.db.Transaction(func() (err error) {
		var flagOwnerId = currentUserID
		var f *types.MessageFlag

		// @todo [SECURITY] verify if current user can access & write to this channel

		if flag == types.MessageFlagPinnedToChannel {
			// It does not matter how is the owner of the pin,
			flagOwnerId = 0
		}

		f, err = svc.mflag.FindByFlag(messageID, flagOwnerId, flag)
		if f.ID == 0 && remove {
			// Skip removing, flag does not exists
			return nil
		} else if f.ID > 0 && !remove {
			// Skip adding, flag already exists
			return nil
		} else if err != nil && err != repository.ErrMessageFlagNotFound {
			// Other errors, exit
			return
		}

		// Check message
		var msg *types.Message
		msg, err = svc.message.FindMessageByID(messageID)
		if err != nil {
			return
		}

		if remove {
			err = svc.mflag.DeleteByID(f.ID)
			f.DeletedAt = timeNowPtr()
		} else {
			f, err = svc.mflag.Create(&types.MessageFlag{
				UserID:    currentUserID,
				ChannelID: msg.ChannelID,
				MessageID: msg.ID,
				Flag:      flag,
			})
		}

		if err != nil {
			return err
		}

		svc.sendFlagEvent(f)

		return
	})

	return errors.Wrap(err, "Can not flag/un-flag message")
}

func (svc *message) preload(mm types.MessageSet) (err error) {
	if err = svc.preloadUsers(mm); err != nil {
		return
	}

	if err = svc.preloadAttachments(mm); err != nil {
		return
	}

	if err = svc.preloadFlags(mm); err != nil {
		return
	}

	if err = svc.preloadMentions(mm); err != nil {
		return
	}

	if err = svc.message.PrefillThreadParticipants(mm); err != nil {
		return
	}

	return
}

// Preload for all messages
func (svc *message) preloadUsers(mm types.MessageSet) (err error) {
	var uu systemTypes.UserSet

	for _, msg := range mm {
		if msg.User != nil || msg.UserID == 0 {
			continue
		}

		if msg.User = uu.FindByID(msg.UserID); msg.User != nil {
			continue
		}

		if msg.User, _ = svc.usr.FindByID(msg.UserID); msg.User != nil {
			// @todo fix this handler errors (ignore user-not-found, return others)
			uu = append(uu, msg.User)
		}
	}

	return
}

// Preload for all messages
func (svc *message) preloadFlags(mm types.MessageSet) (err error) {
	var ff types.MessageFlagSet

	ff, err = svc.mflag.FindByMessageIDs(mm.IDs()...)
	if err != nil {
		return
	}

	return ff.Walk(func(flag *types.MessageFlag) error {
		mm.FindByID(flag.MessageID).Flags = append(mm.FindByID(flag.MessageID).Flags, flag)
		return nil
	})
}

// Preload for all messages
func (svc *message) preloadMentions(mm types.MessageSet) (err error) {
	var mentions types.MentionSet

	mentions, err = svc.mentions.FindByMessageIDs(mm.IDs()...)
	if err != nil {
		return
	}

	return mm.Walk(func(m *types.Message) error {
		m.Mentions = mentions.FindByMessageID(m.ID)
		return nil
	})
}

func (svc *message) preloadAttachments(mm types.MessageSet) (err error) {
	var (
		ids []uint64
		aa  types.MessageAttachmentSet
	)

	err = mm.Walk(func(m *types.Message) error {
		if m.Type == types.MessageTypeAttachment || m.Type == types.MessageTypeInlineImage {
			ids = append(ids, m.ID)
		}
		return nil
	})

	if err != nil {
		return
	}

	if aa, err = svc.attachment.FindAttachmentByMessageID(ids...); err != nil {
		return
	} else {
		return aa.Walk(func(a *types.MessageAttachment) error {
			if a.MessageID > 0 {
				if m := mm.FindByID(a.MessageID); m != nil {
					m.Attachment = &a.Attachment
				}
			}

			return nil
		})
	}
}

// Sends message to event loop
func (svc *message) sendEvent(mm ...*types.Message) (err error) {
	if err = svc.preload(mm); err != nil {
		return
	}

	for _, msg := range mm {
		if msg.User == nil {
			// @todo fix this handler errors (ignore user-not-found, return others)
			msg.User, _ = svc.usr.FindByID(msg.UserID)
		}

		if err = svc.evl.Message(msg); err != nil {
			return
		}
	}

	return
}

// Sends message to event loop
func (svc *message) sendFlagEvent(ff ...*types.MessageFlag) (err error) {
	for _, f := range ff {
		if err = svc.evl.MessageFlag(f); err != nil {
			return
		}
	}

	return
}

func (svc *message) extractMentions(m *types.Message) (mm types.MentionSet) {
	const reSubID = 2
	mm = types.MentionSet{}

	match := mentionsFinder.FindAllStringSubmatch(m.Message, -1)

	// Prepopulated with all we know from message
	tpl := types.Mention{
		ChannelID:     m.ChannelID,
		MessageID:     m.ID,
		MentionedByID: m.UserID,
	}

	for m := 0; m < len(match); m++ {
		uid := payload.ParseUInt64(match[m][reSubID])
		if len(mm.FindByUserID(uid)) == 0 {
			// Copy template & assign user id
			mnt := tpl
			mnt.UserID = uid
			mm = append(mm, &mnt)
		}
	}

	return
}

func (svc *message) updateMentions(messageID uint64, mm types.MentionSet) error {
	if existing, err := svc.mentions.FindByMessageIDs(messageID); err != nil {
		return errors.Wrap(err, "Could not update mentions")
	} else if len(mm) > 0 {
		add, _, del := existing.Diff(mm)

		err = add.Walk(func(m *types.Mention) error {
			m, err = svc.mentions.Create(m)
			return err
		})

		if err != nil {
			return errors.Wrap(err, "Could not create mentions")
		}

		err = del.Walk(func(m *types.Mention) error {
			return svc.mentions.DeleteByID(m.ID)
		})

		if err != nil {
			return errors.Wrap(err, "Could not delete mentions")
		}
	} else {
		return svc.mentions.DeleteByMessageID(messageID)
	}

	return nil
}

var _ MessageService = &message{}
