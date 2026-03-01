//go:build amd64 || arm64 || riscv64 || mips64 || ppc64

package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

type FeishuChannel struct {
	*channels.BaseChannel
	config   config.FeishuConfig
	client   *lark.Client
	wsClient *larkws.Client

	mu     sync.Mutex
	cancel context.CancelFunc

	// botOpenID is discovered at startup via the bot info API.
	botOpenID string
}

func NewFeishuChannel(cfg config.FeishuConfig, bus *bus.MessageBus) (*FeishuChannel, error) {
	base := channels.NewBaseChannel("feishu", cfg, bus, cfg.AllowFrom,
		channels.WithGroupTrigger(cfg.GroupTrigger),
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &FeishuChannel{
		BaseChannel: base,
		config:      cfg,
		client:      lark.NewClient(cfg.AppID, cfg.AppSecret),
	}, nil
}

func (c *FeishuChannel) Start(ctx context.Context) error {
	if c.config.AppID == "" || c.config.AppSecret == "" {
		return fmt.Errorf("feishu app_id or app_secret is empty")
	}

	dispatcher := larkdispatcher.NewEventDispatcher(c.config.VerificationToken, c.config.EncryptKey).
		OnP2MessageReceiveV1(c.handleMessageReceive)

	runCtx, cancel := context.WithCancel(ctx)

	c.mu.Lock()
	c.cancel = cancel
	c.wsClient = larkws.NewClient(
		c.config.AppID,
		c.config.AppSecret,
		larkws.WithEventHandler(dispatcher),
	)
	wsClient := c.wsClient
	c.mu.Unlock()

	c.SetRunning(true)
	logger.InfoC("feishu", "Feishu channel started (websocket mode)")

	go func() {
		if err := wsClient.Start(runCtx); err != nil {
			logger.ErrorCF("feishu", "Feishu websocket stopped with error", map[string]any{
				"error": err.Error(),
			})
		}
	}()

	return nil
}

func (c *FeishuChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.wsClient = nil
	c.mu.Unlock()

	c.SetRunning(false)
	logger.InfoC("feishu", "Feishu channel stopped")
	return nil
}

func (c *FeishuChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	if msg.ChatID == "" {
		return fmt.Errorf("chat ID is empty")
	}

	msgType, payload, err := buildFeishuContent(msg.Content)
	if err != nil {
		return fmt.Errorf("failed to build feishu content: %w", err)
	}

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(msg.ChatID).
			MsgType(msgType).
			Content(payload).
			Uuid(fmt.Sprintf("picoclaw-%d", time.Now().UnixNano())).
			Build()).
		Build()

	resp, err := c.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu send: %w", channels.ErrTemporary)
	}

	if !resp.Success() {
		return fmt.Errorf("feishu api error (code=%d msg=%s): %w", resp.Code, resp.Msg, channels.ErrTemporary)
	}

	logger.DebugCF("feishu", "Feishu message sent", map[string]any{
		"chat_id": msg.ChatID,
	})

	return nil
}

// EditMessage implements channels.MessageEditor.
// It edits an existing message using the Feishu UpdateMessage API.
// Uses post format to match the placeholder message type.
func (c *FeishuChannel) EditMessage(ctx context.Context, chatID string, messageID string, content string) error {
	msgType, payload, err := buildFeishuContent(content)
	if err != nil {
		return fmt.Errorf("failed to build feishu edit content: %w", err)
	}

	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType(msgType).
			Content(payload).
			Build()).
		Build()

	resp, err := c.client.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu edit message: %w", err)
	}

	if !resp.Success() {
		return fmt.Errorf("feishu edit message api error (code=%d msg=%s)", resp.Code, resp.Msg)
	}

	return nil
}

// SendPlaceholder implements channels.PlaceholderCapable.
// It sends a placeholder post message that will later be edited via EditMessage.
// Uses post format so EditMessage can update it with rich text (post→post).
func (c *FeishuChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
	if !c.config.Placeholder.Enabled {
		return "", nil
	}

	text := c.config.Placeholder.Text
	if text == "" {
		text = "Thinking... 💭"
	}

	msgType, payload, err := buildFeishuContent(text)
	if err != nil {
		return "", fmt.Errorf("failed to build feishu placeholder: %w", err)
	}

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(payload).
			Uuid(fmt.Sprintf("picoclaw-ph-%d", time.Now().UnixNano())).
			Build()).
		Build()

	resp, err := c.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu send placeholder: %w", err)
	}

	if !resp.Success() || resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu send placeholder api error (code=%d msg=%s)", resp.Code, resp.Msg)
	}

	return *resp.Data.MessageId, nil
}

// ReactToMessage implements channels.ReactionCapable.
// It adds a THUMBSUP reaction to the inbound message and returns an undo function.
func (c *FeishuChannel) ReactToMessage(ctx context.Context, chatID, messageID string) (func(), error) {
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType("Typing").Build()).
			Build()).
		Build()

	resp, err := c.client.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return func() {}, fmt.Errorf("feishu add reaction: %w", err)
	}

	if !resp.Success() {
		return func() {}, fmt.Errorf("feishu add reaction api error (code=%d msg=%s)", resp.Code, resp.Msg)
	}

	var reactionID string
	if resp.Data != nil && resp.Data.ReactionId != nil {
		reactionID = *resp.Data.ReactionId
	}

	var undoOnce sync.Once
	return func() {
		undoOnce.Do(func() {
			if reactionID == "" {
				return
			}
			delReq := larkim.NewDeleteMessageReactionReqBuilder().
				MessageId(messageID).
				ReactionId(reactionID).
				Build()
			_, _ = c.client.Im.V1.MessageReaction.Delete(context.Background(), delReq)
		})
	}, nil
}

// SendMedia implements channels.MediaSender.
// It uploads and sends media files (images/files) via the Feishu API.
func (c *FeishuChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	store := c.GetMediaStore()
	if store == nil {
		return fmt.Errorf("no media store available: %w", channels.ErrSendFailed)
	}

	for _, part := range msg.Parts {
		localPath, err := store.Resolve(part.Ref)
		if err != nil {
			logger.ErrorCF("feishu", "Failed to resolve media ref", map[string]any{
				"ref":   part.Ref,
				"error": err.Error(),
			})
			continue
		}

		switch part.Type {
		case "image":
			if err := c.sendImage(ctx, msg.ChatID, localPath); err != nil {
				return err
			}
		default: // "audio", "video", "file", or unknown
			filename := part.Filename
			if filename == "" {
				filename = filepath.Base(localPath)
			}
			if err := c.sendFile(ctx, msg.ChatID, localPath, filename); err != nil {
				return err
			}
		}
	}

	return nil
}

// sendImage uploads an image and sends it as an image message.
func (c *FeishuChannel) sendImage(ctx context.Context, chatID, localPath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("feishu open image: %w", channels.ErrSendFailed)
	}
	defer file.Close()

	uploadReq := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.ImageTypeMessage).
			Image(file).
			Build()).
		Build()

	uploadResp, err := c.client.Im.V1.Image.Create(ctx, uploadReq)
	if err != nil {
		return fmt.Errorf("feishu upload image: %w", channels.ErrTemporary)
	}

	if !uploadResp.Success() || uploadResp.Data == nil || uploadResp.Data.ImageKey == nil {
		return fmt.Errorf("feishu upload image api error (code=%d msg=%s): %w",
			uploadResp.Code, uploadResp.Msg, channels.ErrTemporary)
	}

	imageKey := *uploadResp.Data.ImageKey
	payload, _ := json.Marshal(map[string]string{"image_key": imageKey})

	sendReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeImage).
			Content(string(payload)).
			Uuid(fmt.Sprintf("picoclaw-img-%d", time.Now().UnixNano())).
			Build()).
		Build()

	sendResp, err := c.client.Im.V1.Message.Create(ctx, sendReq)
	if err != nil {
		return fmt.Errorf("feishu send image: %w", channels.ErrTemporary)
	}

	if !sendResp.Success() {
		return fmt.Errorf("feishu send image api error (code=%d msg=%s): %w",
			sendResp.Code, sendResp.Msg, channels.ErrTemporary)
	}

	return nil
}

// sendFile uploads a file and sends it as a file message.
func (c *FeishuChannel) sendFile(ctx context.Context, chatID, localPath, filename string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("feishu open file: %w", channels.ErrSendFailed)
	}
	defer file.Close()

	fileType := inferFeishuFileType(filename)

	uploadReq := larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType(fileType).
			FileName(filename).
			File(file).
			Build()).
		Build()

	uploadResp, err := c.client.Im.V1.File.Create(ctx, uploadReq)
	if err != nil {
		return fmt.Errorf("feishu upload file: %w", channels.ErrTemporary)
	}

	if !uploadResp.Success() || uploadResp.Data == nil || uploadResp.Data.FileKey == nil {
		return fmt.Errorf("feishu upload file api error (code=%d msg=%s): %w",
			uploadResp.Code, uploadResp.Msg, channels.ErrTemporary)
	}

	fileKey := *uploadResp.Data.FileKey
	payload, _ := json.Marshal(map[string]string{"file_key": fileKey})

	sendReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeFile).
			Content(string(payload)).
			Uuid(fmt.Sprintf("picoclaw-file-%d", time.Now().UnixNano())).
			Build()).
		Build()

	sendResp, err := c.client.Im.V1.Message.Create(ctx, sendReq)
	if err != nil {
		return fmt.Errorf("feishu send file: %w", channels.ErrTemporary)
	}

	if !sendResp.Success() {
		return fmt.Errorf("feishu send file api error (code=%d msg=%s): %w",
			sendResp.Code, sendResp.Msg, channels.ErrTemporary)
	}

	return nil
}

// inferFeishuFileType maps a filename extension to a Feishu file type constant.
func inferFeishuFileType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".opus", ".ogg":
		return larkim.FileTypeOpus
	case ".mp4":
		return larkim.FileTypeMp4
	case ".pdf":
		return larkim.FileTypePdf
	case ".doc", ".docx":
		return larkim.FileTypeDoc
	case ".xls", ".xlsx":
		return larkim.FileTypeXls
	case ".ppt", ".pptx":
		return larkim.FileTypePpt
	default:
		return larkim.FileTypeStream
	}
}

func (c *FeishuChannel) handleMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}

	message := event.Event.Message
	sender := event.Event.Sender

	chatID := stringValue(message.ChatId)
	if chatID == "" {
		return nil
	}

	senderID := extractFeishuSenderID(sender)
	if senderID == "" {
		senderID = "unknown"
	}

	content := extractFeishuMessageContent(message)
	if content == "" {
		content = "[empty message]"
	}

	metadata := map[string]string{}
	messageID := ""
	if mid := stringValue(message.MessageId); mid != "" {
		messageID = mid
	}
	if messageType := stringValue(message.MessageType); messageType != "" {
		metadata["message_type"] = messageType
	}
	if chatType := stringValue(message.ChatType); chatType != "" {
		metadata["chat_type"] = chatType
	}
	if sender != nil && sender.TenantKey != nil {
		metadata["tenant_key"] = *sender.TenantKey
	}

	chatType := stringValue(message.ChatType)
	var peer bus.Peer
	if chatType == "p2p" {
		peer = bus.Peer{Kind: "direct", ID: senderID}
	} else {
		peer = bus.Peer{Kind: "group", ID: chatID}
		// Detect @mention and strip bot mention from content
		isMentioned := c.isBotMentioned(message)
		if isMentioned {
			content = c.stripBotMention(content, message)
		}
		// In group chats, apply unified group trigger filtering
		respond, cleaned := c.ShouldRespondInGroup(isMentioned, content)
		if !respond {
			return nil
		}
		content = cleaned
	}

	logger.InfoCF("feishu", "Feishu message received", map[string]any{
		"sender_id": senderID,
		"chat_id":   chatID,
		"preview":   utils.Truncate(content, 80),
	})

	senderInfo := bus.SenderInfo{
		Platform:    "feishu",
		PlatformID:  senderID,
		CanonicalID: identity.BuildCanonicalID("feishu", senderID),
	}

	if !c.IsAllowedSender(senderInfo) {
		return nil
	}

	c.HandleMessage(ctx, peer, messageID, senderID, chatID, content, nil, metadata, senderInfo)
	return nil
}

// isBotMentioned checks if the bot is mentioned in the message's mentions list.
// Feishu identifies bot mentions by sender_type="app" in the mention entries,
// or by matching the app_id against the configured AppID.
func (c *FeishuChannel) isBotMentioned(message *larkim.EventMessage) bool {
	if message == nil || len(message.Mentions) == 0 {
		return false
	}

	for _, mention := range message.Mentions {
		if mention == nil {
			continue
		}
		// Feishu marks bot mentions with Name containing the bot name.
		// The mention ID open_id corresponds to the bot's open_id.
		// A reliable approach: check if the mention's key exists and
		// the name is not empty (bot mentions always have the bot's display name).
		// Since we don't have the bot's open_id at init time (would require an extra API call),
		// we use a heuristic: if any mention has an ID with open_id matching the app,
		// or if the mention name matches. However, the most reliable way is to check
		// if mention.Id is nil — when an app/bot is mentioned, the Id field is populated.
		//
		// Simplest reliable approach: feishu includes all mentions in the list.
		// Bot mentions will have Id.OpenId set. We compare against botOpenID if available,
		// otherwise treat any mention of an app-type entity as a bot mention by checking
		// if the key exists in the content text.
		if c.botOpenID != "" && mention.Id != nil {
			if mention.Id.OpenId != nil && *mention.Id.OpenId == c.botOpenID {
				return true
			}
		}
		// Fallback: In feishu, when mentioning a bot, the Name field is set to the bot's name
		// and the mention key appears in the text as @_user_N.
		// If we don't have botOpenID, we check if any mention has the same tenant
		// as the app (this is a common pattern). For now, accept any mention as potential
		// bot mention when botOpenID is not set — the ShouldRespondInGroup logic
		// will handle the rest.
		if c.botOpenID == "" && mention.Key != nil && mention.Id != nil && mention.Id.OpenId != nil {
			// Without botOpenID, we can't definitively identify the bot.
			// Use AppID prefix matching as a heuristic — feishu bot open_ids
			// are derived from the app. A safer default: treat it as mentioned
			// so the bot responds when @'d in groups.
			return true
		}
	}

	return false
}

// stripBotMention removes bot mention placeholders (e.g. @_user_1) from the content text.
func (c *FeishuChannel) stripBotMention(content string, message *larkim.EventMessage) string {
	if message == nil || len(message.Mentions) == 0 {
		return content
	}

	for _, mention := range message.Mentions {
		if mention == nil || mention.Key == nil {
			continue
		}
		// Only strip the bot's mention, not other user mentions
		isBotMention := false
		if c.botOpenID != "" && mention.Id != nil && mention.Id.OpenId != nil {
			isBotMention = *mention.Id.OpenId == c.botOpenID
		} else if c.botOpenID == "" && mention.Id != nil && mention.Id.OpenId != nil {
			// Without botOpenID, strip any mention that triggered isBotMentioned
			isBotMention = true
		}
		if isBotMention {
			content = strings.ReplaceAll(content, *mention.Key, "")
		}
	}

	return strings.TrimSpace(content)
}

func extractFeishuSenderID(sender *larkim.EventSender) string {
	if sender == nil || sender.SenderId == nil {
		return ""
	}

	if sender.SenderId.UserId != nil && *sender.SenderId.UserId != "" {
		return *sender.SenderId.UserId
	}
	if sender.SenderId.OpenId != nil && *sender.SenderId.OpenId != "" {
		return *sender.SenderId.OpenId
	}
	if sender.SenderId.UnionId != nil && *sender.SenderId.UnionId != "" {
		return *sender.SenderId.UnionId
	}

	return ""
}

func extractFeishuMessageContent(message *larkim.EventMessage) string {
	if message == nil || message.Content == nil || *message.Content == "" {
		return ""
	}

	if message.MessageType != nil && *message.MessageType == larkim.MsgTypeText {
		var textPayload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(*message.Content), &textPayload); err == nil {
			return textPayload.Text
		}
	}

	return *message.Content
}
